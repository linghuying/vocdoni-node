package circuit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

const (
	// FilenameProvingKey defines the name of the file of the circom ProvingKey
	FilenameProvingKey = "proving_key.zkey"
	// FilenameVerificationKey defines the name of the file of the circom
	// VerificationKey
	FilenameVerificationKey = "verification_key.json"
	// FilenameWasm defines the name of the file of the circuit wasm compiled
	//version
	FilenameWasm = "circuit.wasm"
)

// ZkCircuitConfig defines the configuration of the files to be downloaded
type ZkCircuitConfig struct {
	// URI defines the URI from where to download the files
	URI string `json:"uri"`
	// CircuitPath defines the path from where the files are downloaded
	CircuitPath string `json:"circuitPath"`
	// Parameters used for the circuit build
	Parameters []int64 `json:"parameters"`
	Levels     int     `json:"levels"`
	// LocalDir defines in which directory will be the files
	// downloaded, under that directory it will follow the CircuitPath
	// directories structure
	LocalDir string `json:"localDir"`

	// WasmHash contains the expected hash for the file filenameWasm
	WasmHash []byte `json:"wasmHash"` // circuit.wasm
	// ProvingKeyHash contains the expected hash for the file filenameZKey
	ProvingKeyHash []byte `json:"zKeyHash"` // proving_key.zkey
	// VerificationKeyHash contains the expected hash for the file filenameVK
	VerificationKeyHash []byte `json:"vKHash"` // verification_key.json
}

// ZkCircuit struct wraps the circuit configuration and contains the file
// content of the circuit artifacts (provingKey, verificationKey and wasm)
type ZkCircuit struct {
	ProvingKey      []byte
	VerificationKey []byte
	Wasm            []byte

	Config ZkCircuitConfig
}

// LoadZkCircuit function load the circuit artifacts based on the configuration
// provided. First, tries to load the artifacts from local storage, if they are
// not available, tries to download from their remote location. Then,
func LoadZkCircuit(ctx context.Context, config ZkCircuitConfig) (*ZkCircuit, error) {
	circuit := &ZkCircuit{Config: config}

	// load the artifacts of the provided circuit from the local storage
	if err := circuit.LoadLocal(); err == nil {
		// tries to verify the loaded artifacts, if it success, returns the
		// loaded circuit, else continue.
		correct, err := circuit.VerifiedCircuitArtifacts()
		if err == nil && correct {
			return circuit, nil
		}
	}

	// if the circuit is not available locally, tries to download from its
	// remote location
	if err := circuit.LoadRemote(ctx); err != nil {
		return nil, err
	}

	// checks hashes of current files
	if correct, err := circuit.VerifiedCircuitArtifacts(); err != nil {
		return nil, err
	} else if !correct {
		return nil, fmt.Errorf("download artifacts does not match with the expected ones")
	}

	return circuit, nil
}

// LoadLocal tries to read the content of current circuit artifacts from its
// local path (provingKey, verificationKey and wasm). If any of the read
// operatios fails, returns an error.
func (circuit *ZkCircuit) LoadLocal() error {
	var err error

	// compose files localpath
	provingKeyLocalPath := filepath.Join(circuit.Config.LocalDir,
		circuit.Config.CircuitPath, FilenameProvingKey)
	verificationKeyLocalPath := filepath.Join(circuit.Config.LocalDir,
		circuit.Config.CircuitPath, FilenameVerificationKey)
	wasmLocalPath := filepath.Join(circuit.Config.LocalDir,
		circuit.Config.CircuitPath, FilenameWasm)

	// read file contents into circuit parameters
	circuit.ProvingKey, err = os.ReadFile(provingKeyLocalPath)
	if err != nil {
		return fmt.Errorf("error reading provingKey locally: %w", err)
	}

	circuit.VerificationKey, err = os.ReadFile(verificationKeyLocalPath)
	if err != nil {
		return fmt.Errorf("error reading verificationKey locally: %w", err)
	}

	circuit.Wasm, err = os.ReadFile(wasmLocalPath)
	if err != nil {
		return fmt.Errorf("error reading wasm circuit locally: %w", err)
	}

	return nil
}

// LoadRemote downloads the content of the current circuit artifacts from its
// remote location. If any of the downloads fails, returns an error.
func (circuit *ZkCircuit) LoadRemote(ctx context.Context) error {
	baseUri, err := url.Parse(circuit.Config.URI)
	if err != nil {
		return err
	}

	remotePath := fmt.Sprintf("%s/%s", baseUri.String(), circuit.Config.CircuitPath)
	localPath := filepath.Join(circuit.Config.LocalDir, circuit.Config.CircuitPath)
	if err := os.MkdirAll(localPath, os.ModePerm); err != nil {
		return err
	}

	// Compose provingKey remote and local locations
	provingKeyUri := fmt.Sprintf("%s/%s", remotePath, FilenameProvingKey)
	provingKeyLocalPath := filepath.Join(localPath, FilenameProvingKey)
	// Compose verificationKey remote and local locations
	verificationKeyUri := fmt.Sprintf("%s/%s", remotePath, FilenameVerificationKey)
	verificationKeyLocalPath := filepath.Join(localPath, FilenameVerificationKey)
	// Compose wasm remote and local locations
	wasmUri := fmt.Sprintf("%s/%s", remotePath, FilenameWasm)
	wasmLocalPath := filepath.Join(localPath, FilenameWasm)

	// Download and store locally provingKey
	circuit.ProvingKey, err = downloadFile(ctx, provingKeyUri)
	if err != nil {
		return fmt.Errorf("error downloading provingKey: %w", err)
	} else if err := storeFile(circuit.ProvingKey, provingKeyLocalPath); err != nil {
		return fmt.Errorf("error storing provingKey: %w", err)
	}

	// Download and store locally verificationKey
	circuit.VerificationKey, err = downloadFile(ctx, verificationKeyUri)
	if err != nil {
		return fmt.Errorf("error downloading verificationKey: %w", err)
	} else if err := storeFile(circuit.VerificationKey, verificationKeyLocalPath); err != nil {
		return fmt.Errorf("error storing verificationKey: %w", err)
	}

	// Download and store locally wasm circuit
	circuit.Wasm, err = downloadFile(ctx, wasmUri)
	if err != nil {
		return fmt.Errorf("error downloading wasm circuit: %w", err)
	} else if err := storeFile(circuit.Wasm, wasmLocalPath); err != nil {
		return fmt.Errorf("error storing wasm circuit: %w", err)
	}

	return nil
}

// VerifiedCircuitArtifacts function checks that the computed hash of every
// circuit artifact matches with the expected hash, from the circuit config.
func (circuit *ZkCircuit) VerifiedCircuitArtifacts() (bool, error) {
	zKeyVerified, err := checkHash(circuit.ProvingKey, circuit.Config.ProvingKeyHash)
	if err != nil {
		return false, err
	}

	vKeyVerified, err := checkHash(circuit.VerificationKey, circuit.Config.VerificationKeyHash)
	if err != nil {
		return false, err
	}

	wasmVerified, err := checkHash(circuit.Wasm, circuit.Config.WasmHash)
	if err != nil {
		return false, err
	}

	return zKeyVerified && vKeyVerified && wasmVerified, nil
}

// checkHash compute the hash of the content provided and compares it with the
// hash provided as expected result. It returns a boolean with the result of the
// comparation and with an error.
func checkHash(content, expected []byte) (bool, error) {
	if content == nil {
		return false, fmt.Errorf("no content provided to check")
	} else if expected == nil {
		return false, fmt.Errorf("no hash provided to compare")
	}

	hash := sha256.New()
	if n, err := hash.Write(content); err != nil {
		return false, fmt.Errorf("error computing hash function of %s: %w", content, err)
	} else if n != len(content) {
		return false, fmt.Errorf("the number of writted bytes does not match with the content provied")
	}

	return bytes.Equal(hash.Sum(nil), expected), nil
}

// downloadFile functions perform a GET request to the URL provided and returns
// the content of the received response. If something fails returns an error.
func downloadFile(ctx context.Context, fileUrl string) ([]byte, error) {
	if _, err := url.Parse(fileUrl); err != nil {
		return nil, fmt.Errorf("error parsing the file URL provided: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileUrl, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating the file request: %w", err)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	} else if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error on download file %s: http status: %d", fileUrl, res.StatusCode)
	}

	defer res.Body.Close()
	content, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading the file content from the http response: %w", err)
	}

	return content, nil
}

// storeFile helper function allows to write the file content provided into a
// new file created at the path provided.
func storeFile(content []byte, dstPath string) error {
	if content == nil {
		return fmt.Errorf("no content provided")
	} else if _, err := os.Stat(filepath.Dir(dstPath)); err != nil {
		return fmt.Errorf("destination path parent folder does not exist")
	}

	fd, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("something was wrong creating the artifact file: %w", err)
	}

	if nBytes, err := fd.Write(content); err != nil {
		return fmt.Errorf("something was wrong writting the artifact file: %w", err)
	} else if len(content) != nBytes {
		return fmt.Errorf("something was wrong writting the artifact file: the length of the provided content does not match with the bytes writted")
	}

	return nil
}
