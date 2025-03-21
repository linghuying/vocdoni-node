package indexer

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.vocdoni.io/dvote/log"
	"go.vocdoni.io/dvote/statedb"
	"go.vocdoni.io/dvote/types"
	"go.vocdoni.io/dvote/vochain"
	indexerdb "go.vocdoni.io/dvote/vochain/indexer/db"
	"go.vocdoni.io/dvote/vochain/indexer/indexertypes"
	"go.vocdoni.io/dvote/vochain/results"
	"go.vocdoni.io/dvote/vochain/state"
	"go.vocdoni.io/dvote/vochain/transaction/vochaintx"
	"go.vocdoni.io/proto/build/go/models"

	"github.com/pressly/goose/v3"

	// modernc is a pure-Go version, but its errors have less useful info.
	// We use mattn while developing and testing, and we can swap them later.
	// _ "modernc.org/sqlite"
	_ "github.com/mattn/go-sqlite3"
)

//go:generate go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0 generate

//go:embed migrations/*.sql
var embedMigrations embed.FS

const dbFilename = "db.sqlite"

// EventListener is an interface used for executing custom functions during the
// events of the tally of a process.
type EventListener interface {
	OnComputeResults(results *results.Results, process *indexertypes.Process, height uint32)
}

// AddEventListener adds a new event listener, to receive method calls on block
// events as documented in EventListener.
func (idx *Indexer) AddEventListener(l EventListener) {
	idx.eventOnResults = append(idx.eventOnResults, l)
}

// Indexer is the component which makes the accounting of the voting processes
// and keeps it indexed in a local database.
type Indexer struct {
	App *vochain.BaseApplication

	// votePool is the set of votes that should be live counted,
	// first grouped by processId, then keyed by nullifier.
	// Only keeping one vote per nullifier is important for "overwrite" votes,
	// so that we only count the last one in the live results.
	// TODO: try using blockTx directly, after some more refactors?
	votePool map[string]map[string]*state.Vote

	dbPath      string
	readOnlyDB  *sql.DB
	readWriteDB *sql.DB

	readOnlyQuery *indexerdb.Queries

	// blockMu protects blockTx, blockQueries, and blockUpdateProcs.
	blockMu sync.Mutex
	// blockTx is an in-progress SQL transaction which is committed or rolled
	// back along with the current block.
	blockTx *sql.Tx
	// blockQueries wraps blockTx. Note that it is kept between multiple transactions
	// so that we can reuse the same prepared statements.
	blockQueries *indexerdb.Queries
	// blockUpdateProcs is the list of process IDs that require sync with the state database.
	// The key is a types.ProcessID as a string, so that it can be used as a map key.
	blockUpdateProcs          map[string]bool
	blockUpdateProcVoteCounts map[string]bool

	// list of live processes (those on which the votes will be computed on arrival)
	// TODO: we could query the procs table, perhaps memoizing to avoid querying the same over and over again?
	liveResultsProcs sync.Map

	// eventOnResults is the list of external callbacks that will be executed by the indexer
	eventOnResults []EventListener

	// ignoreLiveResults if true, partial/live results won't be calculated (only final results)
	ignoreLiveResults bool
}

type Options struct {
	DataDir string

	// ExpectBackupRestore should be set to true if a call to Indexer.RestoreBackup
	// will be made shortly after New is called, and before any indexing or queries happen.
	// If the DB file on disk exists, this flag will be ignored and the existing DB will be loaded.
	ExpectBackupRestore bool

	IgnoreLiveResults bool
}

// New returns an instance of the Indexer
// using the local storage database in DataDir and integrated into the state vochain instance.
func New(app *vochain.BaseApplication, opts Options) (*Indexer, error) {
	idx := &Indexer{
		App:               app,
		ignoreLiveResults: opts.IgnoreLiveResults,

		// TODO(mvdan): these three maps are all keyed by process ID,
		// and each of them needs to query existing data from the DB.
		// Since the map keys very often overlap, consider joining the maps
		// so that we can also reuse queries to the DB.
		votePool:                  make(map[string]map[string]*state.Vote),
		blockUpdateProcs:          make(map[string]bool),
		blockUpdateProcVoteCounts: make(map[string]bool),
	}
	log.Infow("indexer initialization", "dataDir", opts.DataDir, "liveResults", !opts.IgnoreLiveResults)

	// The DB itself is opened in "rwc" mode, so it is created if it does not yet exist.
	// Create the parent directory as well if it doesn't exist.
	if err := os.MkdirAll(opts.DataDir, os.ModePerm); err != nil {
		return nil, err
	}
	idx.dbPath = filepath.Join(opts.DataDir, dbFilename)

	// if dbPath exists, always startDB (ExpectBackupRestore is ignored)
	// if dbPath doesn't exist, and we're not expecting a BackupRestore, startDB
	// if dbPath doesn't exist and we're expecting a backup, skip startDB, it will be triggered after the restore
	if _, err := os.Stat(idx.dbPath); err == nil ||
		(os.IsNotExist(err) && !opts.ExpectBackupRestore) {
		if err := idx.startDB(); err != nil {
			return nil, err
		}
	}

	// Subscribe to events
	idx.App.State.AddEventListener(idx)

	return idx, nil
}

func (idx *Indexer) startDB() error {
	if idx.readWriteDB != nil {
		panic("Indexer.startDB called twice")
	}

	var err error

	// sqlite doesn't support multiple concurrent writers.
	// For that reason, readWriteDB is limited to one open connection.
	// Per https://github.com/mattn/go-sqlite3/issues/1022#issuecomment-1067353980,
	// we use WAL to allow multiple concurrent readers at the same time.
	idx.readWriteDB, err = sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=rwc&_journal_mode=wal&_txlock=immediate&_synchronous=normal&_foreign_keys=true", idx.dbPath))
	if err != nil {
		return err
	}
	idx.readWriteDB.SetMaxOpenConns(1)
	idx.readWriteDB.SetMaxIdleConns(1)
	idx.readWriteDB.SetConnMaxIdleTime(10 * time.Minute)

	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	goose.SetLogger(log.GooseLogger())
	goose.SetBaseFS(embedMigrations)

	if gooseMigrationsPending(idx.readWriteDB, "migrations") {
		log.Info("indexer db needs migration, scheduling a reindex after sync")
		defer func() { go idx.ReindexBlocks(false) }()
	}

	if err := goose.Up(idx.readWriteDB, "migrations"); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	// Analyze the tables and indices and store information in internal tables
	// so that the query optimizer can make better choices.
	if _, err := idx.readWriteDB.Exec("PRAGMA analysis_limit=1000; ANALYZE"); err != nil {
		return err
	}

	idx.readOnlyDB, err = sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro&_journal_mode=wal", idx.dbPath))
	if err != nil {
		return err
	}
	// Increasing these numbers can allow for more queries to run concurrently,
	// but it also increases the memory used by sqlite and our connection pool.
	// Most read-only queries we run are quick enough, so a small number seems OK.
	idx.readOnlyDB.SetMaxOpenConns(16)
	idx.readOnlyDB.SetMaxIdleConns(4)
	idx.readOnlyDB.SetConnMaxIdleTime(30 * time.Minute)

	idx.readOnlyQuery, err = indexerdb.Prepare(context.TODO(), idx.readOnlyDB)
	if err != nil {
		return err
	}
	idx.blockQueries, err = indexerdb.Prepare(context.TODO(), idx.readWriteDB)
	if err != nil {
		return err
	}
	return nil
}

func copyFile(dst, src string) error {
	srcf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcf.Close()

	// For now, we don't care about permissions
	dstf, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(dstf, srcf)
	if err2 := dstf.Close(); err == nil {
		err = err2
	}
	return err
}

func (idx *Indexer) Close() error {
	if err := idx.readOnlyDB.Close(); err != nil {
		return err
	}
	if err := idx.readWriteDB.Close(); err != nil {
		return err
	}
	return nil
}

// RestoreBackup restores the database from a backup created via SaveBackup.
// Note that this must be called with ExpectBackupRestore set to true,
// and before any indexing or queries happen.
func (idx *Indexer) RestoreBackup(path string) error {
	if idx.readWriteDB != nil {
		panic("Indexer.RestoreBackup called after the database was initialized")
	}
	if err := copyFile(idx.dbPath, path); err != nil {
		return fmt.Errorf("could not restore indexer backup: %w", err)
	}
	if err := idx.startDB(); err != nil {
		return err
	}
	return nil
}

func gooseMigrationsPending(db *sql.DB, dir string) bool {
	// Get the latest applied migration version
	currentVersion, err := goose.GetDBVersion(db)
	if err != nil {
		log.Errorf("failed to get current database version: %v", err)
		return false
	}

	// Collect migrations after the current version
	migrations, err := goose.CollectMigrations(dir, currentVersion, goose.MaxVersion)
	if err != nil {
		if errors.Is(err, goose.ErrNoMigrationFiles) {
			return false
		}
		log.Errorf("failed to collect migrations: %v", err)
		return false
	}

	return len(migrations) > 0
}

// SaveBackup backs up the database to a file on disk.
// Note that writes to the database may be blocked until the backup finishes,
// and an error may occur if a file at path already exists.
//
// For sqlite, this is done via "VACUUM INTO", so the resulting file is also a database.
func (idx *Indexer) SaveBackup(ctx context.Context, path string) error {
	_, err := idx.readOnlyDB.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// ExportBackupAsBytes backs up the database, and returns the contents as []byte.
//
// Note that writes to the database may be blocked until the backup finishes.
//
// For sqlite, this is done via "VACUUM INTO", so the resulting file is also a database.
func (idx *Indexer) ExportBackupAsBytes(ctx context.Context) ([]byte, error) {
	tmpDir, err := os.MkdirTemp("", "indexer")
	if err != nil {
		return nil, fmt.Errorf("error creating tmpDir: %w", err)
	}
	tmpFilePath := filepath.Join(tmpDir, "indexer.sqlite3")
	if err := idx.SaveBackup(ctx, tmpFilePath); err != nil {
		return nil, fmt.Errorf("error saving indexer backup: %w", err)
	}
	defer func() {
		if err := os.Remove(tmpFilePath); err != nil {
			log.Warnw("error removing indexer backup file", "path", tmpFilePath, "err", err)
		}
	}()
	return os.ReadFile(tmpFilePath)
}

// blockTxQueries assumes that lockPool is locked.
func (idx *Indexer) blockTxQueries() *indexerdb.Queries {
	if idx.blockMu.TryLock() {
		panic("Indexer.blockTxQueries was called without locking Indexer.lockPool")
	}
	if idx.blockTx == nil {
		tx, err := idx.readWriteDB.Begin()
		if err != nil {
			panic(err) // shouldn't happen, use an error return if it ever does
		}
		idx.blockTx = tx
		idx.blockQueries = idx.blockQueries.WithTx(tx)
	}
	return idx.blockQueries
}

// AfterSyncBootstrap is a blocking function that waits until the Vochain is synchronized
// and then execute a set of recovery actions. It mainly checks for those processes which are
// still open (live) and updates all temporary data (current voting weight and live results
// if unecrypted). This method might be called on a goroutine after initializing the Indexer.
// TO-DO: refactor and use blockHeight for reusing existing live results
func (idx *Indexer) AfterSyncBootstrap(inTest bool) {
	// if no live results, we don't need the bootstraping
	if idx.ignoreLiveResults {
		return
	}

	if !inTest {
		<-idx.App.WaitUntilSynced()
	}

	log.Infof("running indexer after-sync bootstrap")

	// Note that holding blockMu means new votes aren't added until the recovery finishes.
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()

	queries := idx.blockTxQueries()
	ctx := context.TODO()

	prcIDs, err := queries.GetProcessIDsByFinalResults(ctx, false)
	if err != nil {
		log.Error(err)
		return // no point in continuing further
	}

	log.Infof("recovered %d live results processes", len(prcIDs))
	log.Infof("starting live results recovery computation")
	startTime := time.Now()
	for _, p := range prcIDs {
		// In order to recover the full list of live results, we need
		// to reset the existing Results and count them again from scratch.
		// Since we cannot be sure if there are votes missing, we need to
		// perform the full computation.
		log.Debugf("recovering live process %x", p)
		process, err := idx.ProcessInfo(p)
		if err != nil {
			log.Errorf("cannot fetch process: %v", err)
			continue
		}
		options := process.VoteOpts

		indxR := &results.Results{
			ProcessID: p,
			// MaxValue requires +1 since 0 is also an option
			Votes:        results.NewEmptyVotes(options),
			Weight:       new(types.BigInt).SetUint64(0),
			VoteOpts:     options,
			EnvelopeType: process.Envelope,
		}

		if _, err := queries.UpdateProcessResultByID(ctx, indexerdb.UpdateProcessResultByIDParams{
			ID:       indxR.ProcessID,
			Votes:    indexertypes.EncodeJSON(indxR.Votes),
			Weight:   indexertypes.EncodeJSON(indxR.Weight),
			VoteOpts: indexertypes.EncodeProto(indxR.VoteOpts),
			Envelope: indexertypes.EncodeProto(indxR.EnvelopeType),
		}); err != nil {
			log.Errorw(err, "cannot UpdateProcessResultByID sql")
			continue
		}

		// Count the votes, add them to partialResults (in memory, without any db transaction)
		partialResults := &results.Results{
			Weight:       new(types.BigInt).SetUint64(0),
			VoteOpts:     options,
			EnvelopeType: process.Envelope,
		}
		// Get the votes from the state
		if err := idx.App.State.IterateVotes(p, true, func(vote *models.StateDBVote) bool {
			if err := idx.addLiveVote(process, vote.VotePackage, new(big.Int).SetBytes(vote.Weight), partialResults); err != nil {
				log.Errorw(err, "could not add live vote")
			}
			return false
		}); err != nil {
			if errors.Is(err, statedb.ErrEmptyTree) {
				log.Debugf("process %x doesn't have any votes yet, skipping", p)
				continue
			}
			log.Errorw(err, "unexpected error during iterate votes")
			continue
		}

		// Store the results on the persistent database
		if err := idx.commitVotesUnsafe(queries, p, indxR, partialResults, nil, idx.App.Height()); err != nil {
			log.Errorw(err, "could not commit live votes")
			continue
		}
		// Add process to live results so new votes will be added
		idx.addProcessToLiveResults(p)
	}

	// don't wait until the next Commit call to commit blockTx
	if err := idx.blockTx.Commit(); err != nil {
		log.Errorw(err, "could not commit tx")
	}
	idx.blockTx = nil

	log.Infof("live results recovery computation finished, took %s", time.Since(startTime))
}

// ReindexBlocks reindexes all blocks found in blockstore
func (idx *Indexer) ReindexBlocks(inTest bool) {
	if !inTest {
		<-idx.App.WaitUntilSynced()
	}

	// Note that holding blockMu means new votes aren't added until the reindex finishes.
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()

	if idx.App.Node == nil || idx.App.Node.BlockStore() == nil {
		return
	}

	idxBlockCount, err := idx.CountBlocks("", "", "")
	if err != nil {
		log.Warnf("indexer CountBlocks returned error: %s", err)
	}
	log.Infow("start reindexing",
		"blockStoreBase", idx.App.Node.BlockStore().Base(),
		"blockStoreHeight", idx.App.Node.BlockStore().Height(),
		"indexerBlockCount", idxBlockCount,
	)
	queries := idx.blockTxQueries()
	for height := idx.App.Node.BlockStore().Base(); height <= idx.App.Node.BlockStore().Height(); height++ {
		if b := idx.App.GetBlockByHeight(int64(height)); b != nil {
			// Blocks
			func() {
				idxBlock, err := idx.readOnlyQuery.GetBlockByHeight(context.TODO(), b.Height)
				if height%10000 == 1 {
					log.Infof("reindexing height %d, updating values (%s, %x, %x, %x) on current row %+v",
						height, b.ChainID, b.Hash(), b.ProposerAddress, b.LastBlockID.Hash, idxBlock)
					if err := idx.blockTx.Commit(); err != nil {
						log.Errorw(err, "could not commit tx")
					}
					idx.blockTx = nil
					queries = idx.blockTxQueries()
				}
				if err == nil && idxBlock.Time != b.Time {
					log.Errorf("while reindexing blocks, block %d timestamp in db (%s) differs from blockstore (%s), leaving untouched", height, idxBlock.Time, b.Time)
					return
				}
				if _, err := queries.CreateBlock(context.TODO(), indexerdb.CreateBlockParams{
					ChainID:         b.ChainID,
					Height:          b.Height,
					Time:            b.Time,
					Hash:            nonNullBytes(b.Hash()),
					ProposerAddress: nonNullBytes(b.ProposerAddress),
					LastBlockHash:   nonNullBytes(b.LastBlockID.Hash),
				}); err != nil {
					log.Errorw(err, "cannot index new block")
				}
			}()

			// Transactions
			func() {
				for index, tx := range b.Data.Txs {
					idxTx, err := idx.readOnlyQuery.GetTransactionByHeightAndIndex(context.TODO(), indexerdb.GetTransactionByHeightAndIndexParams{
						BlockHeight: b.Height,
						BlockIndex:  int64(index),
					})
					if err == nil && !bytes.Equal(idxTx.Hash, tx.Hash()) {
						log.Errorf("while reindexing txs, tx %d/%d hash in db (%x) differs from blockstore (%x), leaving untouched", b.Height, index, idxTx.Hash, tx.Hash())
						return
					}
					vtx := new(vochaintx.Tx)
					if err := vtx.Unmarshal(tx, b.ChainID); err != nil {
						log.Errorw(err, fmt.Sprintf("cannot unmarshal tx %d/%d", b.Height, index))
						continue
					}
					idx.indexTx(vtx, uint32(b.Height), int32(index))
				}
			}()
		}
	}

	if err := idx.blockTx.Commit(); err != nil {
		log.Errorw(err, "could not commit tx")
	}
	idx.blockTx = nil

	log.Infow("finished reindexing",
		"blockStoreBase", idx.App.Node.BlockStore().Base(),
		"blockStoreHeight", idx.App.Node.BlockStore().Height(),
		"indexerBlockCount", idxBlockCount,
	)
}

// Commit is called by the APP when a block is confirmed and included into the chain
func (idx *Indexer) Commit(height uint32) error {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()

	// Update existing processes
	updateProcs := slices.Sorted(maps.Keys(idx.blockUpdateProcs))

	queries := idx.blockTxQueries()
	ctx := context.TODO()

	// index the new block
	if b := idx.App.GetBlockByHeight(int64(height)); b != nil {
		if _, err := queries.CreateBlock(context.TODO(), indexerdb.CreateBlockParams{
			ChainID:         b.ChainID,
			Height:          b.Height,
			Time:            b.Time,
			Hash:            nonNullBytes(b.Hash()),
			ProposerAddress: nonNullBytes(b.ProposerAddress),
			LastBlockHash:   nonNullBytes(b.LastBlockID.Hash),
		}); err != nil {
			log.Errorw(err, "cannot index new block")
		}
	}

	for _, pidStr := range updateProcs {
		pid := types.ProcessID(pidStr)
		if err := idx.updateProcess(ctx, queries, pid); err != nil {
			log.Errorw(err, "commit: cannot update process")
			continue
		}
		log.Debugw("updated process", "processID", hex.EncodeToString(pid))
	}
	clear(idx.blockUpdateProcs)

	// Add votes collected by onVote (live results)
	newVotes := 0
	overwritedVotes := 0
	startTime := time.Now()

	for pidStr, votesByNullifier := range idx.votePool {
		pid := []byte(pidStr)
		// Get the process information while reusing blockTx
		procInner, err := queries.GetProcess(ctx, pid)
		if err != nil {
			log.Warnf("cannot get process %x", pid)
			continue
		}
		proc := indexertypes.ProcessFromDB(&procInner)

		// results is used to accumulate the new votes for a process
		addedResults := &results.Results{
			Weight:       new(types.BigInt).SetUint64(0),
			VoteOpts:     proc.VoteOpts,
			EnvelopeType: proc.Envelope,
		}
		// subtractedResults is used to subtract votes that are overwritten
		subtractedResults := &results.Results{
			Weight:       new(types.BigInt).SetUint64(0),
			VoteOpts:     proc.VoteOpts,
			EnvelopeType: proc.Envelope,
		}
		// The order here isn't deterministic, but we assume that to be OK.
		for _, v := range votesByNullifier {
			// If overwrite is 1 or more, we need to update the vote (remove the previous
			// one and add the new) to results.
			// We fetch the previous vote from the state by setting committed=true.
			// Note that if there wasn't a previous vote in the committed state,
			// then it wasn't counted in the results yet, so don't add it to subtractedResults.
			// TODO: can we get previousVote from sqlite via blockTx?
			var previousVote *models.StateDBVote
			if v.Overwrites > 0 {
				previousVote, err = idx.App.State.Vote(v.ProcessID, v.Nullifier, true)
				if err != nil {
					log.Warnw("cannot get previous vote",
						"nullifier", hex.EncodeToString(v.Nullifier),
						"processID", hex.EncodeToString(v.ProcessID),
						"error", err.Error())
				}
			}
			if previousVote != nil {
				log.Debugw("vote overwrite, previous vote",
					"overwrites", v.Overwrites,
					"package", string(previousVote.VotePackage))
				// ensure that overwriteCounter has increased
				if v.Overwrites <= previousVote.GetOverwriteCount() {
					log.Errorw(fmt.Errorf(
						"state stored overwrite count is equal or smaller than current vote overwrite count (%d <= %d)",
						v.Overwrites, previousVote.GetOverwriteCount()),
						"check vote overwrite failed")
					continue
				}
				// add the live vote to subtracted results
				if err := idx.addLiveVote(proc, previousVote.VotePackage,
					new(big.Int).SetBytes(previousVote.Weight), subtractedResults); err != nil {
					log.Errorw(err, "vote cannot be added to subtracted results")
					continue
				}
				overwritedVotes++
			} else {
				newVotes++
			}
			// add the new vote to results
			if err := idx.addLiveVote(proc, v.VotePackage, v.Weight, addedResults); err != nil {
				log.Errorw(err, "vote cannot be added to results")
				continue
			}
		}
		// Commit votes (store to disk)
		if err := idx.commitVotesUnsafe(queries, pid, proc.Results(), addedResults, subtractedResults, idx.App.Height()); err != nil {
			log.Errorf("cannot commit live votes from block %d: (%v)", err, height)
		}
	}
	clear(idx.votePool)

	// Note that we re-compute each process vote count from the votes table,
	// since simply incrementing the vote count would break with vote overwrites.
	for pidStr := range idx.blockUpdateProcVoteCounts {
		pid := []byte(pidStr)
		if _, err := queries.ComputeProcessVoteCount(ctx, pid); err != nil {
			log.Errorw(err, "could not compute process vote count")
		}
	}
	clear(idx.blockUpdateProcVoteCounts)

	if err := idx.blockTx.Commit(); err != nil {
		log.Errorw(err, "could not commit tx")
	}
	idx.blockTx = nil
	if height%1000 == 0 {
		// Regularly see if sqlite thinks another optimization analysis would be useful.
		// Block times tend to be in the order of seconds like 10s,
		// so a thousand blocks will tend to be in the order of hours.
		if _, err := idx.readWriteDB.Exec("PRAGMA optimize"); err != nil {
			return err
		}
	}

	if newVotes+overwritedVotes > 0 {
		log.Infow("add live votes to results",
			"block", height, "newVotes", newVotes, "overwritedVotes",
			overwritedVotes, "time", time.Since(startTime))
	}

	return nil
}

// Rollback removes the non committed pending operations
func (idx *Indexer) Rollback() {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	clear(idx.votePool)
	clear(idx.blockUpdateProcs)
	clear(idx.blockUpdateProcVoteCounts)
	if idx.blockTx != nil {
		if err := idx.blockTx.Rollback(); err != nil {
			log.Errorw(err, "could not rollback tx")
		}
		idx.blockTx = nil
	}
}

// OnProcess indexer stores the processID
func (idx *Indexer) OnProcess(p *models.Process, _ int32) {
	pid := p.GetProcessId()
	if err := idx.newEmptyProcess(pid); err != nil {
		log.Errorw(err, "commit: cannot create new empty process")
	}
	if idx.App.IsSynced() {
		idx.addProcessToLiveResults(pid)
	}
	log.Debugw("new process", "processID", hex.EncodeToString(pid))
}

// OnVote indexer stores the votes if the processId is live results (on going)
// and the blockchain is not synchronizing.
// voterID is the identifier of the voter, the most common case is an ethereum address
// but can be any kind of id expressed as bytes.
func (idx *Indexer) OnVote(vote *state.Vote, txIndex int32) {
	pid := string(vote.ProcessID)
	if !idx.ignoreLiveResults && idx.isProcessLiveResults(vote.ProcessID) {
		// Since []byte in Go isn't comparable, but we can convert any bytes to string.
		nullifier := string(vote.Nullifier)
		if idx.votePool[pid] == nil {
			idx.votePool[pid] = make(map[string]*state.Vote)
		}
		prevVote := idx.votePool[pid][nullifier]
		if prevVote != nil && vote.Overwrites < prevVote.Overwrites {
			log.Warnw("OnVote called with a lower overwrite value than before",
				"previous", prevVote.Overwrites, "latest", vote.Overwrites)
		}
		idx.votePool[pid][nullifier] = vote
	}

	ctx := context.TODO()
	weightStr := `"1"`
	if vote.Weight != nil {
		weightStr = indexertypes.EncodeJSON((*types.BigInt)(vote.Weight))
	}
	keyIndexes := indexertypes.EncodeJSON(vote.EncryptionKeyIndexes)

	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	queries := idx.blockTxQueries()
	if _, err := queries.CreateVote(ctx, indexerdb.CreateVoteParams{
		Nullifier:            vote.Nullifier,
		ProcessID:            vote.ProcessID,
		BlockHeight:          int64(vote.Height),
		BlockIndex:           int64(txIndex),
		Weight:               weightStr,
		OverwriteCount:       int64(vote.Overwrites),
		VoterID:              nonNullBytes(vote.VoterID),
		EncryptionKeyIndexes: keyIndexes,
		Package:              string(vote.VotePackage),
	}); err != nil {
		log.Errorw(err, "could not index vote")
	}
	idx.blockUpdateProcVoteCounts[pid] = true
}

// OnCancel indexer stores the processID and entityID
func (idx *Indexer) OnCancel(pid []byte, _ int32) {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	idx.blockUpdateProcs[string(pid)] = true
}

// OnProcessKeys does nothing
func (idx *Indexer) OnProcessKeys(pid []byte, _ string, _ int32) {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	idx.blockUpdateProcs[string(pid)] = true
}

// OnProcessStatusChange adds the process to blockUpdateProcs and, if ended, the resultsPool
func (idx *Indexer) OnProcessStatusChange(pid []byte, _ models.ProcessStatus, _ int32) {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	idx.blockUpdateProcs[string(pid)] = true
}

// OnProcessDurationChange adds the process to blockUpdateProcs and, if ended, the resultsPool
func (idx *Indexer) OnProcessDurationChange(pid []byte, _ uint32, _ int32) {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	idx.blockUpdateProcs[string(pid)] = true
}

// OnRevealKeys checks if all keys have been revealed and in such case add the
// process to the results queue
func (idx *Indexer) OnRevealKeys(pid []byte, _ string, _ int32) {
	// TODO: can we get KeyIndex from ProcessInfo? perhaps len(PublicKeys), or adding a new sqlite column?
	p, err := idx.App.State.Process(pid, false)
	if err != nil {
		log.Errorf("cannot fetch process %s from state: (%s)", pid, err)
		return
	}
	if p.KeyIndex == nil {
		log.Errorf("keyindex is nil")
		return
	}
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	idx.blockUpdateProcs[string(pid)] = true
}

// OnProcessResults verifies the results for a process and appends it to blockUpdateProcs
func (idx *Indexer) OnProcessResults(pid []byte, _ *models.ProcessResult, _ int32) {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	idx.blockUpdateProcs[string(pid)] = true
}

// OnProcessesStart adds the processes to blockUpdateProcs.
// This is required to update potential changes when a process is started, such as the census root.
func (idx *Indexer) OnProcessesStart(pids [][]byte) {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	for _, pid := range pids {
		idx.blockUpdateProcs[string(pid)] = true
	}
}

func (idx *Indexer) OnSetAccount(accountAddress []byte, account *state.Account) {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	queries := idx.blockTxQueries()
	if _, err := queries.CreateAccount(context.TODO(), indexerdb.CreateAccountParams{
		Account: accountAddress,
		Balance: int64(account.Balance),
		Nonce:   int64(account.Nonce),
	}); err != nil {
		log.Errorw(err, "cannot index new account")
	}
}

func (idx *Indexer) OnTransferTokens(tx *vochaintx.TokenTransfer) {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	queries := idx.blockTxQueries()
	if _, err := queries.CreateTokenTransfer(context.TODO(), indexerdb.CreateTokenTransferParams{
		TxHash:       tx.TxHash,
		BlockHeight:  int64(idx.App.Height()),
		FromAccount:  tx.FromAddress.Bytes(),
		ToAccount:    tx.ToAddress.Bytes(),
		Amount:       int64(tx.Amount),
		TransferTime: time.Unix(idx.App.Timestamp(), 0),
	}); err != nil {
		log.Errorw(err, "cannot index new transaction")
	}
}

// OnCensusUpdate adds the process to blockUpdateProcs in order to update the census.
// This function call is triggered by the SET_PROCESS_CENSUS tx.
func (idx *Indexer) OnCensusUpdate(pid, _ []byte, _ string, _ uint64) {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	idx.blockUpdateProcs[string(pid)] = true
}

// OnSpendTokens indexes a token spending event.
func (idx *Indexer) OnSpendTokens(address []byte, txType models.TxType, cost uint64, reference string) {
	idx.blockMu.Lock()
	defer idx.blockMu.Unlock()
	queries := idx.blockTxQueries()
	if _, err := queries.CreateTokenFee(context.TODO(), indexerdb.CreateTokenFeeParams{
		FromAccount: address,
		TxType:      strings.ToLower(txType.String()),
		Cost:        int64(cost),
		Reference:   reference,
		SpendTime:   time.Unix(idx.App.Timestamp(), 0),
		BlockHeight: int64(idx.App.Height()),
	}); err != nil {
		log.Errorw(err, "cannot index new token spending")
	}
}

// TokenFeesList returns all the token fees associated with a given transaction type, reference and fromAccount
// (all optional filters), ordered by timestamp and paginated by limit and offset
func (idx *Indexer) TokenFeesList(limit, offset int, txType, reference, fromAccount string) (
	[]*indexertypes.TokenFeeMeta, uint64, error,
) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("invalid value: offset cannot be %d", offset)
	}
	if limit <= 0 {
		return nil, 0, fmt.Errorf("invalid value: limit cannot be %d", limit)
	}
	results, err := idx.readOnlyQuery.SearchTokenFees(context.TODO(), indexerdb.SearchTokenFeesParams{
		Limit:       int64(limit),
		Offset:      int64(offset),
		TxType:      txType,
		Reference:   reference,
		FromAccount: fromAccount,
	})
	if err != nil {
		return nil, 0, err
	}
	list := []*indexertypes.TokenFeeMeta{}
	for _, row := range results {
		list = append(list, &indexertypes.TokenFeeMeta{
			Cost:      uint64(row.Cost),
			From:      row.FromAccount,
			TxType:    row.TxType,
			Height:    uint64(row.BlockHeight),
			Reference: row.Reference,
			Timestamp: row.SpendTime,
		})
	}
	if len(results) == 0 {
		return list, 0, nil
	}
	return list, uint64(results[0].TotalCount), nil
}

// TokenTransfersList returns all the token transfers, made to and/or from a given account
// (all optional filters), ordered by timestamp and paginated by limit and offset
func (idx *Indexer) TokenTransfersList(limit, offset int, fromOrToAccount, fromAccount, toAccount string) (
	[]*indexertypes.TokenTransferMeta, uint64, error,
) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("invalid value: offset cannot be %d", offset)
	}
	if limit <= 0 {
		return nil, 0, fmt.Errorf("invalid value: limit cannot be %d", limit)
	}
	results, err := idx.readOnlyQuery.SearchTokenTransfers(context.TODO(), indexerdb.SearchTokenTransfersParams{
		Limit:           int64(limit),
		Offset:          int64(offset),
		FromOrToAccount: fromOrToAccount,
		FromAccount:     fromAccount,
		ToAccount:       toAccount,
	})
	if err != nil {
		return nil, 0, err
	}
	list := []*indexertypes.TokenTransferMeta{}
	for _, row := range results {
		list = append(list, &indexertypes.TokenTransferMeta{
			Amount:    uint64(row.Amount),
			From:      row.FromAccount,
			To:        row.ToAccount,
			Height:    uint64(row.BlockHeight),
			TxHash:    row.TxHash,
			Timestamp: row.TransferTime,
		})
	}
	if len(results) == 0 {
		return list, 0, nil
	}
	return list, uint64(results[0].TotalCount), nil
}

// CountTokenTransfersByAccount returns the count all the token transfers made from a given account
func (idx *Indexer) CountTokenTransfersByAccount(acc []byte) (uint64, error) {
	count, err := idx.readOnlyQuery.CountTokenTransfersByAccount(context.TODO(), acc)
	return uint64(count), err
}

// CountTotalAccounts returns the total number of accounts indexed.
func (idx *Indexer) CountTotalAccounts() (uint64, error) {
	count, err := idx.readOnlyQuery.CountAccounts(context.TODO())
	return uint64(count), err
}

// AccountList returns a list of accounts, accountID is a partial or full hex string,
// and is optional (declared as zero-value will be ignored).
func (idx *Indexer) AccountList(limit, offset int, accountID string) ([]*indexertypes.Account, uint64, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("invalid value: offset cannot be %d", offset)
	}
	if limit <= 0 {
		return nil, 0, fmt.Errorf("invalid value: limit cannot be %d", limit)
	}
	results, err := idx.readOnlyQuery.SearchAccounts(context.TODO(), indexerdb.SearchAccountsParams{
		Limit:           int64(limit),
		Offset:          int64(offset),
		AccountIDSubstr: accountID,
	})
	if err != nil {
		return nil, 0, err
	}
	list := []*indexertypes.Account{}
	for _, row := range results {
		list = append(list, &indexertypes.Account{
			Address: row.Account,
			Balance: uint64(row.Balance),
			Nonce:   uint32(row.Nonce),
		})
	}
	if len(results) == 0 {
		return list, 0, nil
	}
	return list, uint64(results[0].TotalCount), nil
}

// AccountExists returns whether the passed accountID exists in the db.
// If passed arg is not the full hex string, returns false (i.e. no substring matching)
func (idx *Indexer) AccountExists(accountID string) bool {
	if len(accountID) != 40 {
		return false
	}
	_, count, err := idx.AccountList(1, 0, accountID)
	if err != nil {
		log.Errorw(err, "indexer query failed")
	}
	return count > 0
}
