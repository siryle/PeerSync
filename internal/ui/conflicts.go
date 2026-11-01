package ui

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/syncer"
)

func (c *Controller) share(shareID string) (config.Share, error) {
	cfg, _, err := config.Load(filepath.Join(c.home, config.FileName))
	if err != nil {
		return config.Share{}, err
	}
	for _, sh := range cfg.Shares {
		if sh.ShareID == shareID {
			return sh, nil
		}
	}
	return config.Share{}, errors.New("找不到这个文件夹")
}

func (c *Controller) Conflicts(shareID string) ([]syncer.Conflict, error) {
	sh, err := c.share(shareID)
	if err != nil {
		return nil, err
	}
	return syncer.ListConflicts(config.IndexPath(c.home, sh.ShareID))
}

func (c *Controller) Conflict(shareID, id string) (syncer.ConflictView, error) {
	sh, err := c.share(shareID)
	if err != nil {
		return syncer.ConflictView{}, err
	}
	return syncer.InspectConflict(sh.Path, config.IndexPath(c.home, sh.ShareID), id)
}

func (c *Controller) DownloadConflict(shareID, id string) (syncer.Conflict, *os.File, error) {
	sh, err := c.share(shareID)
	if err != nil {
		return syncer.Conflict{}, nil, err
	}
	return syncer.OpenConflict(config.IndexPath(c.home, sh.ShareID), id)
}

// Restore pauses sync so the operator can inspect the restored version.
func (c *Controller) RestoreConflict(shareID, id, expected string) (err error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	sh, err := c.share(shareID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	wasRunning := c.running
	c.mu.Unlock()
	if wasRunning {
		c.stop()
	}
	defer func() {
		if err != nil && wasRunning {
			c.start()
		}
	}()
	e, err := syncer.Open(sh.Path, config.IndexPath(c.home, sh.ShareID), c.deviceID, c.log,
		syncer.Options{Mode: sh.Mode, SourceID: sh.SourceID})
	if err != nil {
		return err
	}
	defer e.Close()
	if err = e.Scan(); err != nil {
		return err
	}
	return e.RestoreConflict(id, expected)
}
