package agentstorage

import (
	"fmt"
	"os"
	"time"

	"github.com/uber-go/tally"

	"code.uber.internal/infra/kraken/core"
	"code.uber.internal/infra/kraken/lib/store"
	"code.uber.internal/infra/kraken/lib/torrent/storage"
	"code.uber.internal/infra/kraken/tracker/metainfoclient"
)

// TorrentArchive is capable of initializing torrents in the download directory
// and serving torrents from either the download or cache directory.
type TorrentArchive struct {
	config         Config
	stats          tally.Scope
	fs             store.FileStore
	metaInfoClient metainfoclient.Client
}

// NewTorrentArchive creates a new TorrentArchive.
func NewTorrentArchive(
	config Config,
	stats tally.Scope,
	fs store.FileStore,
	mic metainfoclient.Client) *TorrentArchive {

	config = config.applyDefaults()

	stats = stats.Tagged(map[string]string{
		"module": "agenttorrentarchive",
	})

	return &TorrentArchive{config, stats, fs, mic}
}

// DefaultTorrentArchive returns the default TorrentArchive.
func DefaultTorrentArchive(
	stats tally.Scope, fs store.FileStore, mic metainfoclient.Client) *TorrentArchive {

	return NewTorrentArchive(Config{}, stats, fs, mic)
}

// Stat returns TorrentInfo for given file name. Returns os.ErrNotExist if the
// file does not exist. Ignores namespace.
func (a *TorrentArchive) Stat(namespace, name string) (*storage.TorrentInfo, error) {
	downloadOrCache := a.fs.States().Download().Cache()

	raw, err := downloadOrCache.GetMetadata(name, store.NewTorrentMeta())
	if err != nil {
		return nil, err
	}
	mi, err := core.DeserializeMetaInfo(raw)
	if err != nil {
		return nil, fmt.Errorf("deserialize metainfo: %s", err)
	}

	raw, err = downloadOrCache.GetMetadata(name, store.NewPieceStatus())
	if err != nil {
		return nil, err
	}
	b := newBitfieldFromPieceStatusBytes(name, raw)

	return storage.NewTorrentInfo(mi, b), nil
}

func (a *TorrentArchive) downloadMetaInfo(namespace, name string) (mi *core.MetaInfo, err error) {
	for i := 0; i < a.config.UnavailableMetaInfoRetries; i++ {
		if i > 0 {
			time.Sleep(a.config.UnavailableMetaInfoRetrySleep)
		}
		mi, err = a.metaInfoClient.Download(namespace, name)
		if err != nil {
			if err == metainfoclient.ErrNotFound {
				return nil, storage.ErrNotFound
			}
			continue
		}
		return mi, nil
	}
	return nil, fmt.Errorf("download metainfo: %s", err)
}

// CreateTorrent returns a Torrent for either an existing metainfo / file on
// disk, or downloads metainfo and initializes the file. Returns ErrNotFound
// if no metainfo was found.
func (a *TorrentArchive) CreateTorrent(namespace, name string) (storage.Torrent, error) {
	downloadOrCache := a.fs.States().Download().Cache()

	miRaw, err := downloadOrCache.GetMetadata(name, store.NewTorrentMeta())
	if os.IsNotExist(err) {
		downloadTimer := a.stats.Timer("metainfo_download").Start()
		mi, err := a.downloadMetaInfo(namespace, name)
		if err != nil {
			return nil, err
		}
		downloadTimer.Stop()

		// There's a race condition here, but it's "okay"... Basically, we could
		// initialize a download file with metainfo that is rejected by file store,
		// because someone else beats us to it. However, we catch a lucky break
		// because the only piece of metainfo we use is file length -- which digest
		// (i.e. name) is derived from, so it's "okay".
		if err := a.fs.EnsureDownloadOrCacheFilePresent(mi.Name(), mi.Info.Length); err != nil {
			return nil, fmt.Errorf("ensure download/cache file present: %s", err)
		}
		miRaw, err = mi.Serialize()
		if err != nil {
			return nil, fmt.Errorf("serialize downloaded metainfo: %s", err)
		}
		miRaw, err = downloadOrCache.GetOrSetMetadata(name, store.NewTorrentMeta(), miRaw)
		if err != nil {
			return nil, fmt.Errorf("get or set metainfo: %s", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("get metainfo: %s", err)
	}
	mi, err := core.DeserializeMetaInfo(miRaw)
	if err != nil {
		return nil, fmt.Errorf("parse metainfo: %s", err)
	}

	t, err := NewTorrent(a.fs, mi)
	if err != nil {
		return nil, fmt.Errorf("initialize torrent: %s", err)
	}
	return t, nil
}

// GetTorrent returns a Torrent for an existing metainfo / file on disk. Ignores namespace.
func (a *TorrentArchive) GetTorrent(namespace, name string) (storage.Torrent, error) {
	downloadOrCache := a.fs.States().Download().Cache()

	miRaw, err := downloadOrCache.GetMetadata(name, store.NewTorrentMeta())
	if err != nil {
		return nil, fmt.Errorf("get metainfo: %s", err)
	}
	mi, err := core.DeserializeMetaInfo(miRaw)
	if err != nil {
		return nil, fmt.Errorf("parse metainfo: %s", err)
	}
	t, err := NewTorrent(a.fs, mi)
	if err != nil {
		return nil, fmt.Errorf("initialize torrent: %s", err)
	}
	return t, nil
}

// DeleteTorrent deletes a torrent from disk.
func (a *TorrentArchive) DeleteTorrent(name string) error {
	if err := a.fs.DeleteDownloadOrCacheFile(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
