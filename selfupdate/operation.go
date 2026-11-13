package selfupdate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Operation binds acknowledgement to one verified installation, not merely a
// different version observed after an SSE disconnect. It contains no credentials.
type Operation struct {
	ID             string `json:"operation_id"`
	RequestID      string `json:"request_id"`
	Repository     string `json:"repository"`
	FromVersion    string `json:"from_version"`
	TargetVersion  string `json:"target_version"`
	Asset          string `json:"asset"`
	AcceptedBootID string `json:"accepted_boot_id"`
	BinarySHA256   string `json:"binary_sha256,omitempty"`
	Phase          string `json:"phase"`
	Error          string `json:"error,omitempty"`
	UpdatedAt      int64  `json:"updated_at"`
}

var processBootID = uuid.NewString()

func BootID() string { return processBootID }

var runtimeDigest struct {
	sync.Once
	value string
}

// Compute once per process, before a later upgrade can replace the on-disk path.
func RuntimeBinarySHA256() string {
	runtimeDigest.Do(func() {
		if p, err := ResolvePaths(); err == nil {
			runtimeDigest.value, _ = fileSHA256(p.Current)
		}
	})
	return runtimeDigest.value
}

func operationPath(p Paths) string {
	return strings.TrimSuffix(p.Marker, ".upgrade.json") + ".update-operation.json"
}

func LoadOperation() (Operation, bool) {
	p, err := ResolvePaths()
	if err != nil {
		return Operation{}, false
	}
	return readOperation(p)
}

func readOperation(p Paths) (Operation, bool) {
	data, err := os.ReadFile(operationPath(p))
	var op Operation
	if err != nil || len(data) > 64<<10 || json.Unmarshal(data, &op) != nil || op.ID == "" {
		return Operation{}, false
	}
	return op, true
}

func SaveOperation(op Operation) error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	return saveOperation(p, op)
}

func saveOperation(p Paths, op Operation) error {
	op.UpdatedAt = time.Now().Unix()
	return writeUpdateJSON(operationPath(p), op)
}

func operationResult(p Paths, m marker, phase, detail string) error {
	if m.Operation == nil {
		return nil // legacy markers have no trustworthy operation identity
	}
	op := *m.Operation
	op.Phase, op.Error = phase, detail
	return saveOperation(p, op)
}

// A same-directory temporary file preserves the last complete marker/receipt if
// writing or replacement fails. Never remove the old marker to make rename work.
func writeUpdateJSON(path string, value any) (err error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gondar-update-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(name) }()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func settleOperation(p Paths, m marker) error {
	if m.Operation == nil {
		return nil
	}
	if m.Attempts == 0 {
		return errors.New("更新尚未进入目标程序的稳定启动阶段")
	}
	sum, err := fileSHA256(p.Current)
	if err != nil || m.Operation.BinarySHA256 == "" || sum != m.Operation.BinarySHA256 {
		return errors.New("运行程序尚未匹配已验签发行物")
	}
	return operationResult(p, m, "settling", "")
}
