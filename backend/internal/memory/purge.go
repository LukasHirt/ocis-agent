package memory

import (
	"context"
	"errors"
	"fmt"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Purge deletes the agent's folder (/.ocis-agent/, memory and README) from the
// user's space and forgets the user's cached ETags. A missing folder is fine,
// so Purge can be repeated. The deletion goes to the oCIS trash.
func (s *Service) Purge(ctx context.Context, sess Session) error {
	err := s.files.Delete(ctx, sess.Creds, sess.DriveID, Root)
	if err != nil && !errors.Is(err, ocisclient.ErrNotFound) {
		return fmt.Errorf("memory: delete %s: %w", Root, err)
	}
	err = s.st.InTx(ctx, func(tx store.Tx) error {
		c, err := tx.GetCursor(sess.UserID)
		if err != nil {
			return err
		}
		if len(c.MemoryETags) == 0 {
			return nil
		}
		c.MemoryETags = map[string]string{}
		return tx.PutCursor(c)
	})
	if err != nil {
		return fmt.Errorf("memory: forget cached ETags: %w", err)
	}
	return nil
}
