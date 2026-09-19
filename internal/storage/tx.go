package storage

import (
	"context"
	"database/sql"
	"fmt"
)

type WriteTx struct {
	tx *sql.Tx
}

func (s *Store) BeginWrite(ctx context.Context) (*WriteTx, error) {
	// Write connection was opened with _txlock=immediate in DSN
	tx, err := s.writeDB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin immediate write tx: %w", err)
	}
	return &WriteTx{tx: tx}, nil
}

func (tx *WriteTx) Commit() error {
	return tx.tx.Commit()
}

func (tx *WriteTx) Rollback() error {
	return tx.tx.Rollback()
}

func (tx *WriteTx) Tx() *sql.Tx {
	return tx.tx
}
