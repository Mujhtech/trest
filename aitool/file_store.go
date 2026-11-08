package aitool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const maxRecordBytes = 4 << 20

var recordID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// FileStore is a single-owner durable store for local/dev deployments on POSIX
// systems. It takes a nonblocking OS file lock for its whole lifetime; a second
// process cannot execute the same approvals. Each update fsyncs a private file,
// atomically renames it, and fsyncs the directory. Use a transactional database
// implementation of Store for distributed deployment or network filesystems.
type FileStore struct {
	mu       sync.Mutex
	dir      string
	lock     *os.File
	isClosed bool
}

// OpenFileStore requires a private dedicated directory (0700), creates it if
// absent, takes exclusive ownership, and marks interrupted running records as
// uncertain. It never makes those records executable again.
func OpenFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("aitool: store must be a private directory with mode 0700")
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(lock); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("aitool: store is already owned or locking is unavailable: %w", err)
	}
	s := &FileStore{dir: dir, lock: lock}
	entries, err := os.ReadDir(dir)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-5]
		r, err := s.load(id)
		if err != nil {
			_ = s.Close()
			return nil, err
		}
		if r.State == Running {
			r.State = Uncertain
			r.UpdatedAt = time.Now().UTC()
			r.Failure = &Failure{Code: "unavailable", Message: "process interrupted; reconcile effects or provider usage before retry"}
			if err := s.write(r); err != nil {
				_ = s.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed {
		return nil
	}
	s.isClosed = true
	return s.lock.Close()
}
func (s *FileStore) Create(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed {
		return errors.New("aitool: store is closed")
	}
	if _, err := s.load(r.ID); err == nil {
		return ErrRecordExists
	} else if !errors.Is(err, ErrRecordNotFound) {
		return err
	}
	return s.write(r)
}
func (s *FileStore) Load(id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed {
		return Record{}, errors.New("aitool: store is closed")
	}
	return s.load(id)
}
func (s *FileStore) Update(id string, fn func(*Record) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed {
		return errors.New("aitool: store is closed")
	}
	r, err := s.load(id)
	if err != nil {
		return err
	}
	if err := fn(&r); err != nil {
		return err
	}
	if r.ID != id {
		return errors.New("aitool: update cannot change record identity")
	}
	return s.write(r)
}
func (s *FileStore) load(id string) (Record, error) {
	if !recordID.MatchString(id) {
		return Record{}, errors.New("aitool: invalid record id")
	}
	path := filepath.Join(s.dir, id+".json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrRecordNotFound
	}
	if err != nil {
		return Record{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return Record{}, errors.New("aitool: record must be a private regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return Record{}, err
	}
	if len(data) > maxRecordBytes {
		return Record{}, errors.New("aitool: record exceeds size limit")
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, err
	}
	if r.Version != 1 || r.ID != id {
		return Record{}, errors.New("aitool: invalid record version or identity")
	}
	return r, nil
}
func (s *FileStore) write(r Record) error {
	if !recordID.MatchString(r.ID) || r.Version != 1 {
		return errors.New("aitool: invalid record id or version")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > maxRecordBytes {
		return errors.New("aitool: record exceeds size limit")
	}
	f, err := os.CreateTemp(s.dir, ".pending-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(temp) }()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, filepath.Join(s.dir, r.ID+".json")); err != nil {
		return err
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
