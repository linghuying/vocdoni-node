package apiclient

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/iden3/go-iden3-crypto/babyjub"
	"github.com/iden3/go-iden3-crypto/poseidon"
	"github.com/vocdoni/arbo"
	"go.vocdoni.io/dvote/api"
	"go.vocdoni.io/dvote/crypto/zk"
	"go.vocdoni.io/dvote/crypto/zk/prover"
	"go.vocdoni.io/dvote/httprouter/apirest"
	"go.vocdoni.io/dvote/types"
	"go.vocdoni.io/dvote/util"
	"go.vocdoni.io/dvote/vochain"
	"go.vocdoni.io/proto/build/go/models"
	"google.golang.org/protobuf/proto"
)

// VoteData contains the data needed to create a vote.
//
// Choices is a list of choices, where each position represents a question.
// ElectionID is the ID of the election.
// ProofMkTree is the proof of the vote for a off chain tree, weighted election.
// ProofCSP is the proof of the vote fore a CSP election.
//
// KeyType is the type of the key used when the census was created. It can be
// either models.ProofArbo_ADDRESS or models.ProofArbo_PUBKEY (default).
type VoteData struct {
	Choices    []int
	ElectionID types.HexBytes

	ProofMkTree *CensusProof
	ProofCSP    types.HexBytes
	ProofZkTree *CensusProofZk
}

// GetNullifierZk function returns ZkSnark ready vote nullifier and also encodes
// and returns the electionId into a string slice to be used in other processes
// such as proof generation.
func (c *HTTPclient) GetNullifierZk(privKey babyjub.PrivateKey, electionId types.HexBytes) (types.HexBytes, []string, error) {
	// Encode the electionId -> sha256(electionId)
	hashedProcessId := sha256.Sum256(electionId)
	intProcessId := []*big.Int{
		new(big.Int).SetBytes(arbo.SwapEndianness(hashedProcessId[:16])),
		new(big.Int).SetBytes(arbo.SwapEndianness(hashedProcessId[16:])),
	}
	strProcessId := []string{intProcessId[0].String(), intProcessId[1].String()}

	// Calculate nullifier hash: poseidon(babyjubjub(privKey) + sha256(processId))
	nullifier, err := poseidon.Hash([]*big.Int{
		babyjub.SkToBigInt(&privKey),
		intProcessId[0],
		intProcessId[1],
	})
	if err != nil {
		return nil, nil, fmt.Errorf("error generating nullifier: %w", err)
	}

	return nullifier.Bytes(), strProcessId, nil
}

// Vote sends a vote to the Vochain. The vote is a VoteData struct,
// which contains the electionID, the choices and the proof. The
// return value is the fcvoteID (nullifier).
func (c *HTTPclient) Vote(v *VoteData) (types.HexBytes, error) {
	votePackage := &vochain.VotePackage{
		Votes: v.Choices,
	}
	votePackageBytes, err := json.Marshal(votePackage)
	if err != nil {
		return nil, err
	}
	vote := &models.VoteEnvelope{
		Nonce:       util.RandomBytes(16),
		ProcessId:   v.ElectionID,
		VotePackage: votePackageBytes,
	}

	// Get de election metadata
	election, err := c.Election(v.ElectionID)
	if err != nil {
		return nil, err
	}

	// Build the proof
	// TODO: Change the condition to the type of census origin
	switch {
	case election.VoteMode.Anonymous:
		if v.ProofZkTree == nil {
			return nil, fmt.Errorf("no zk proof provided")
		}

		proof, err := prover.ParseProof(v.ProofZkTree.Proof, v.ProofZkTree.PubSignals)
		if err != nil {
			return nil, err
		}

		weight := new(big.Int).SetInt64(int64(v.ProofZkTree.Weight))
		protoProof, err := zk.ProverProofToProtobufZKProof(v.ProofZkTree.CircuitParametersIndex,
			proof, v.ElectionID, election.Census.CensusRoot, v.ProofZkTree.Nullifier, weight)
		if err != nil {
			return nil, err
		}

		vote.Nullifier = v.ProofZkTree.Nullifier
		vote.Proof = &models.Proof{
			Payload: &models.Proof_ZkSnark{
				ZkSnark: protoProof,
			},
		}
	case v.ProofMkTree != nil:
		vote.Proof = &models.Proof{
			Payload: &models.Proof_Arbo{
				Arbo: &models.ProofArbo{
					Type:     models.ProofArbo_BLAKE2B,
					Siblings: v.ProofMkTree.Proof,
					Value:    v.ProofMkTree.Value,
					KeyType:  v.ProofMkTree.KeyType,
				},
			},
		}
	case v.ProofCSP != nil:
		p := models.ProofCA{}
		if err := proto.Unmarshal(v.ProofCSP, &p); err != nil {
			return nil, fmt.Errorf("could not decode CSP proof: %w", err)
		}
		vote.Proof = &models.Proof{
			Payload: &models.Proof_Ca{Ca: &p},
		}
	}

	// Sign and send the vote
	stx := models.SignedTx{}
	stx.Tx, err = proto.Marshal(&models.Tx{
		Payload: &models.Tx_Vote{
			Vote: vote,
		},
	})
	if err != nil {
		return nil, err
	}
	stx.Signature, err = c.account.SignVocdoniTx(stx.Tx, c.ChainID())
	if err != nil {
		return nil, err
	}
	stxb, err := proto.Marshal(&stx)
	if err != nil {
		return nil, err
	}

	voteAPI := &api.Vote{TxPayload: stxb}
	resp, code, err := c.Request("POST", voteAPI, "votes")
	if err != nil {
		return nil, err
	}
	if code != apirest.HTTPstatusCodeOK {
		return nil, fmt.Errorf("%s: %d (%s)", errCodeNot200, code, resp)
	}
	err = json.Unmarshal(resp, &voteAPI)
	if err != nil {
		return nil, fmt.Errorf("could not unmarshal response: %v", err)
	}

	return voteAPI.VoteID, nil
}

// Verify verifies a vote. The voteID is the nullifier of the vote.
func (c *HTTPclient) Verify(electionID, voteID types.HexBytes) (bool, error) {
	resp, code, err := c.Request("GET", nil, "votes", "verify", electionID.String(), voteID.String())
	if err != nil {
		return false, err
	}
	if code == 200 {
		return true, nil
	}
	if code == 404 {
		return false, nil
	}
	return false, fmt.Errorf("%s: %d (%s)", errCodeNot200, code, resp)
}
