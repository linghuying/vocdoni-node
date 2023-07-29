package indexertypes

import (
	"encoding/json"
	"time"

	"go.vocdoni.io/dvote/log"
	"go.vocdoni.io/dvote/types"
	indexerdb "go.vocdoni.io/dvote/vochain/indexer/db"
	"go.vocdoni.io/dvote/vochain/results"
	"go.vocdoni.io/proto/build/go/models"
	"google.golang.org/protobuf/proto"
)

// Process represents an election process handled by the Vochain.
// The indexer Process data type is different from the vochain state data type
// since it is optimized for querying purposes and not for keeping a shared consensus state.
type Process struct {
	ID                types.HexBytes             `json:"processId"`
	EntityID          types.HexBytes             `json:"entityId"`
	StartBlock        uint32                     `json:"startBlock"`
	EndBlock          uint32                     `json:"endBlock"`
	BlockCount        uint32                     `json:"blockCount"`
	CensusRoot        types.HexBytes             `json:"censusRoot"`
	RollingCensusRoot types.HexBytes             `json:"rollingCensusRoot"`
	CensusURI         string                     `json:"censusURI"`
	Metadata          string                     `json:"metadata"`
	CensusOrigin      int32                      `json:"censusOrigin"`
	Status            int32                      `json:"status"`
	Namespace         uint32                     `json:"namespace"`
	Envelope          *models.EnvelopeType       `json:"envelopeType"`
	Mode              *models.ProcessMode        `json:"processMode"`
	VoteOpts          *models.ProcessVoteOptions `json:"voteOptions"`
	QuestionIndex     uint32                     `json:"questionIndex"`
	CreationTime      time.Time                  `json:"creationTime"`
	HaveResults       bool                       `json:"haveResults"`
	FinalResults      bool                       `json:"finalResults"`
	SourceBlockHeight uint64                     `json:"sourceBlockHeight"`
	SourceNetworkId   string                     `json:"sourceNetworkId"` // string form of the enum to be user friendly
	MaxCensusSize     uint64                     `json:"maxCensusSize"`
	RollingCensusSize uint64                     `json:"rollingCensusSize"`

	PrivateKeys json.RawMessage `json:"-"` // json array
	PublicKeys  json.RawMessage `json:"-"` // json array

	VoteCount uint64 `json:"-"` // via LEFT JOIN

	ResultsVotes       [][]*types.BigInt `json:"-"`
	ResultsWeight      *types.BigInt     `json:"-"`
	ResultsBlockHeight uint32            `json:"-"`
}

func (p *Process) Results() *results.Results {
	return &results.Results{
		ProcessID:    p.ID,
		Votes:        p.ResultsVotes,
		Weight:       p.ResultsWeight,
		EnvelopeType: p.Envelope,
		VoteOpts:     p.VoteOpts,
		BlockHeight:  p.ResultsBlockHeight,
	}
}

func ProcessFromDB(dbproc *indexerdb.GetProcessRow) *Process {
	proc := &Process{
		ID:                dbproc.ID,
		EntityID:          nonEmptyBytes(dbproc.EntityID),
		StartBlock:        uint32(dbproc.StartBlock),
		EndBlock:          uint32(dbproc.EndBlock),
		BlockCount:        uint32(dbproc.BlockCount),
		HaveResults:       dbproc.HaveResults,
		FinalResults:      dbproc.FinalResults,
		CensusRoot:        nonEmptyBytes(dbproc.CensusRoot),
		RollingCensusRoot: nonEmptyBytes(dbproc.RollingCensusRoot),
		RollingCensusSize: uint64(dbproc.RollingCensusSize),
		MaxCensusSize:     uint64(dbproc.MaxCensusSize),
		CensusURI:         dbproc.CensusUri,
		CensusOrigin:      int32(dbproc.CensusOrigin),
		Status:            int32(dbproc.Status),
		Namespace:         uint32(dbproc.Namespace),
		CreationTime:      dbproc.CreationTime,
		SourceBlockHeight: uint64(dbproc.SourceBlockHeight),
		Metadata:          dbproc.Metadata,

		PrivateKeys:        json.RawMessage(dbproc.PrivateKeys),
		PublicKeys:         json.RawMessage(dbproc.PublicKeys),
		VoteCount:          uint64(dbproc.VoteCount),
		ResultsVotes:       DecodeJSON[[][]*types.BigInt](dbproc.ResultsVotes),
		ResultsWeight:      DecodeJSON[*types.BigInt](dbproc.ResultsWeight),
		ResultsBlockHeight: uint32(dbproc.ResultsBlockHeight),
	}

	if _, ok := models.SourceNetworkId_name[int32(dbproc.SourceNetworkID)]; !ok {
		log.Errorf("unknown SourceNetworkId: %d", dbproc.SourceNetworkID)
	} else {
		proc.SourceNetworkId = models.SourceNetworkId(dbproc.SourceNetworkID).String()
	}
	proc.Envelope = new(models.EnvelopeType)
	if err := proto.Unmarshal(dbproc.EnvelopePb, proc.Envelope); err != nil {
		log.Error(err)
	}
	proc.Mode = new(models.ProcessMode)
	if err := proto.Unmarshal(dbproc.ModePb, proc.Mode); err != nil {
		log.Error(err)
	}
	proc.VoteOpts = new(models.ProcessVoteOptions)
	if err := proto.Unmarshal(dbproc.VoteOptsPb, proc.VoteOpts); err != nil {
		log.Error(err)
	}
	return proc
}

func EncodeJSON[T any](v T) string {
	p, err := json.Marshal(v)
	if err != nil {
		panic(err) // should not happen
	}
	return string(p)
}

func DecodeJSON[T any](s string) T {
	var v T
	err := json.Unmarshal([]byte(s), &v)
	if err != nil {
		panic(err) // should not happen
	}
	return v
}

func nonEmptyBytes(p []byte) []byte {
	if len(p) == 0 {
		return nil
	}
	return p
}

// EnvelopeMetadata contains vote information for the EnvelopeList api
type EnvelopeMetadata struct {
	ProcessId types.HexBytes `json:"processId"`
	Nullifier types.HexBytes `json:"nullifier"`
	VoterID   types.HexBytes `json:"voterId"`
	TxIndex   int32          `json:"txIndex"`
	Height    uint32         `json:"height"`
	TxHash    types.HexBytes `json:"txHash"`
}

// EnvelopePackage contains a VoteEnvelope and auxiliary information for the Envelope api
type EnvelopePackage struct {
	EncryptionKeyIndexes []uint32         `json:"encryptionKeyIndexes"`
	Meta                 EnvelopeMetadata `json:"meta"`
	Nonce                types.HexBytes   `json:"nonce"`
	Signature            types.HexBytes   `json:"signature"`
	VotePackage          []byte           `json:"votePackage"` // plaintext or encrypted JSON
	Weight               string           `json:"weight"`
	OverwriteCount       uint32           `json:"overwriteCount"`
	Date                 time.Time        `json:"date"`
}

// TxPackage contains a SignedTx and auxiliary information for the Transaction api
type TxPackage struct {
	Tx          []byte         `json:"tx"`
	ID          uint32         `json:"id,omitempty"`
	BlockHeight uint32         `json:"blockHeight,omitempty"`
	Index       *int32         `json:"index,omitempty"`
	Hash        types.HexBytes `json:"hash"`
	Signature   types.HexBytes `json:"signature"`
}

// TxMetadata contains tx information for the TransactionList api
type TxMetadata struct {
	Type        string         `json:"type"`
	BlockHeight uint32         `json:"blockHeight,omitempty"`
	Index       int32          `json:"index"`
	Hash        types.HexBytes `json:"hash"`
}

// Transaction holds the db reference for a single transaction
type Transaction struct {
	Index        uint64         `json:"transactionNumber" format:"int64" example:"944"`
	Hash         types.HexBytes `json:"transactionHash" swaggertype:"string" example:"75e8f822f5dd13973ac5158d600f0a2a5fea4bfefce9712ab5195bf17884cfad"`
	BlockHeight  uint32         `json:"blockHeight" format:"int32" example:"64924"`
	TxBlockIndex int32          `json:"transactionIndex" format:"int32" example:"0"`
	TxType       string         `json:"transactionType" enums:"vote,newProcess,admin,setProcess,registerKey,mintTokens,sendTokens,setTransactionCosts,setAccount,collectFaucet,setKeykeeper" example:"Vote"`
}

func TransactionFromDB(dbtx *indexerdb.Transaction) *Transaction {
	return &Transaction{
		Index:        uint64(dbtx.ID),
		Hash:         dbtx.Hash,
		BlockHeight:  uint32(dbtx.BlockHeight),
		TxBlockIndex: int32(dbtx.BlockIndex),
		TxType:       dbtx.Type,
	}
}

// TokenTransferMeta contains the information of a token transfer and some extra useful information.
// The types are compatible with the SQL defined schema.
type TokenTransferMeta struct {
	Amount    uint64          `json:"amount"`
	From      types.AccountID `json:"from"`
	Height    uint64          `json:"height"`
	TxHash    types.Hash      `json:"txHash"`
	Timestamp time.Time       `json:"timestamp"`
	To        types.AccountID `json:"to"`
}
