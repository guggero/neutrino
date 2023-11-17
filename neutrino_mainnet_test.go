package neutrino

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightninglabs/neutrino/cache/lru"
	"github.com/stretchr/testify/require"
)

func BenchmarkMainnetHeaders(b *testing.B) {
	tempDir := b.TempDir()
	dbName := filepath.Join(tempDir, "neutrino.db")
	db, err := walletdb.Create("bdb", dbName, true, time.Second)
	require.NoError(b, err)

	backendLog := btclog.NewBackend(os.Stdout)
	logger := backendLog.Logger("BTCN")
	logger.SetLevel(btclog.LevelDebug)
	UseLogger(logger)

	b.Cleanup(func() {
		err := db.Close()
		require.NoError(b, err)
	})

	params := &chaincfg.MainNetParams
	blockCache := lru.NewCache[wire.InvVect, *CacheableBlock](20)
	config := Config{
		DataDir:      tempDir,
		Database:     db,
		ChainParams:  *params,
		AddPeers:     []string{},
		ConnectPeers: []string{},
		Dialer: func(addr net.Addr) (net.Conn, error) {
			return net.DialTimeout(
				addr.Network(), addr.String(), time.Second,
			)
		},
		NameResolver: func(host string) ([]net.IP, error) {
			addrs, err := net.LookupHost(host)
			if err != nil {
				return nil, err
			}

			ips := make([]net.IP, 0, len(addrs))
			for _, strIP := range addrs {
				ip := net.ParseIP(strIP)
				if ip == nil {
					continue
				}

				ips = append(ips, ip)
			}

			return ips, nil
		},
		AssertFilterHeader: nil,
		BlockCache:         blockCache,
		BroadcastTimeout:   0,
		PersistToDisk:      true,
		HttpChainSource:    "https://bdn.world",
		UseHttpForHeaders:  true,
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		neutrinoCS, err := NewChainService(config)
		require.NoError(b, err)

		bestBlock, err := neutrinoCS.BestBlock()
		require.NoError(b, err)

		require.Greater(b, bestBlock.Height, int32(800000))
	}
}
