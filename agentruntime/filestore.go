package agentruntime

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileStore is a durable Store that persists each thread as one JSON
// document, written atomically (temp file + fsync + rename) before every
// mutating call returns. Secret and scoped environment values are encrypted
// at rest with AES-256-GCM.
//
// It is a reference implementation aimed at correctness, not throughput:
// every mutation rewrites the thread document.
type FileStore struct {
	*MemoryStore
	dir  string
	aead cipher.AEAD
}

const encPrefix = "enc:v1:"

// OpenFileStore opens (or creates) a FileStore rooted at dir. key must be
// 32 bytes (AES-256) and is used to encrypt sensitive env values at rest.
func OpenFileStore(dir string, key []byte) (*FileStore, error) {
	if len(key) != 32 {
		return nil, errf("filestore: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fs := &FileStore{MemoryStore: NewMemoryStore(), dir: dir, aead: aead}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var td threadData
		if err := json.Unmarshal(b, &td); err != nil {
			return nil, fmt.Errorf("agentruntime: filestore: decode %s: %w", e.Name(), err)
		}
		for k, v := range td.Record.Env {
			if strings.HasPrefix(v.Value, encPrefix) {
				pt, err := fs.decrypt(v.Value)
				if err != nil {
					return nil, fmt.Errorf("agentruntime: filestore: decrypt env %s/%s: %w", td.Record.ID, k, err)
				}
				v.Value = pt
				td.Record.Env[k] = v
			}
		}
		if td.Values == nil {
			td.Values = map[string]json.RawMessage{}
		}
		if td.Files == nil {
			td.Files = map[string]storedFile{}
		}
		if td.Effects == nil {
			td.Effects = map[string]ScheduledEffect{}
		}
		t := td
		fs.threads[td.Record.ID] = &t
	}
	fs.MemoryStore.persist = fs.write
	return fs, nil
}

func (fs *FileStore) write(id string, td *threadData) error {
	if strings.ContainsAny(id, `/\`) || id == "" || id == "." || id == ".." {
		return errf("filestore: invalid thread id %q", id)
	}
	if td == nil {
		err := os.Remove(filepath.Join(fs.dir, id+".json"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	// Copy so encryption never mutates the in-memory plaintext.
	cp := *td
	cp.Record = cloneJSON(td.Record)
	for k, v := range cp.Record.Env {
		if v.sensitive() {
			ct, err := fs.encrypt(v.Value)
			if err != nil {
				return err
			}
			v.Value = ct
			cp.Record.Env[k] = v
		}
	}
	b, err := json.Marshal(&cp)
	if err != nil {
		return err
	}
	final := filepath.Join(fs.dir, id+".json")
	tmp, err := os.CreateTemp(fs.dir, id+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(fs.dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func (fs *FileStore) encrypt(pt string) (string, error) {
	nonce := make([]byte, fs.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := fs.aead.Seal(nonce, nonce, []byte(pt), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(ct), nil
}

func (fs *FileStore) decrypt(s string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, encPrefix))
	if err != nil {
		return "", err
	}
	ns := fs.aead.NonceSize()
	if len(raw) < ns {
		return "", errf("filestore: ciphertext too short")
	}
	pt, err := fs.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
