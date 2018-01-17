// Package repository implements services to work with Git repository storage.
package repository

import (
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"gopkg.in/src-d/go-billy-siva.v4"
	"gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/util"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/storage"
	"gopkg.in/src-d/go-git.v4/storage/filesystem"
)

// RootedTransactioner can initiate transactions on rooted repositories.
type RootedTransactioner interface {
	Begin(h plumbing.Hash) (Tx, error)
}

// Tx is a transaction on a repository. Any change performed in the given
// repository storer is in a transaction context. Transactions are guaranteed
// to be isolated.
type Tx interface {
	// Storer gets the repository storer. It returns the same instance on
	// every call until Commit or Rollback is performed.
	Storer() storage.Storer
	// Commit commits all changes to the repository.
	Commit() error
	// Rollback undoes any changes and cleans up.
	Rollback() error
}

type fsSrv struct {
	copier Copier
	local  billy.Filesystem
}

// NewSivaRootedTransactioner returns a RootedTransactioner for repositories
// stored in the given billy.Filesystem (using siva file format), and uses a
// second billy.Filesystem as temporary storage for in-progress transactions.
//
// Note that transactionality is not fully guaranteed by this implementation,
// since it relies on copying between arbitrary filesystems. If a
// Commit operation fails, the state of the first filesystem is unknown and can
// be invalid.
func NewSivaRootedTransactioner(copier Copier, local billy.Filesystem) RootedTransactioner {
	return &fsSrv{copier, local}
}

func (s *fsSrv) Begin(h plumbing.Hash) (Tx, error) {
	origPath := fmt.Sprintf("%s.siva", h)
	localPath := s.local.Join(
		fmt.Sprintf("%s_%s", h.String(), strconv.FormatInt(time.Now().UnixNano(), 10)))
	localSivaPath := filepath.Join(localPath, "siva")
	localTmpPath := filepath.Join(localPath, "tmp")

	if err := s.copier.CopyFromRemote(origPath, localSivaPath, s.local); err != nil {
		return nil, err
	}

	tmpFs, err := s.local.Chroot(localTmpPath)
	if err != nil {
		return nil, err
	}

	fs, err := sivafs.NewFilesystem(s.local, localSivaPath, tmpFs)
	if err != nil {
		return nil, err
	}

	sto, err := filesystem.NewStorage(fs)
	if err != nil {
		return nil, err
	}

	if _, err := git.Open(sto, nil); err == git.ErrRepositoryNotExists {
		if _, err := git.Init(sto, nil); err != nil {
			return nil, err
		}
	}

	if err != nil {
		return nil, err
	}

	return &fsTx{
		copier:   s.copier,
		local:    s.local,
		sivafs:   fs,
		origPath: origPath,
		tmpPath:  localSivaPath,
		s:        sto,
	}, nil
}

type fsTx struct {
	copier            Copier
	local             billy.Filesystem
	sivafs            sivafs.SivaSync
	tmpPath, origPath string
	s                 storage.Storer
}

func (tx *fsTx) Storer() storage.Storer {
	return tx.s
}

func (tx *fsTx) Commit() error {
	if err := tx.sivafs.Sync(); err != nil {
		return err
	}

	if err := tx.copier.CopyToRemote(tx.tmpPath, tx.origPath, tx.local); err != nil {
		_ = tx.cleanUp()
		return err
	}

	return tx.cleanUp()
}

func (tx *fsTx) Rollback() error {
	return tx.cleanUp()
}

func (tx *fsTx) cleanUp() error {
	path := filepath.Dir(tx.tmpPath)
	return util.RemoveAll(tx.local, path)
}
