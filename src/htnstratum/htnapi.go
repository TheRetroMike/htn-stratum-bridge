package htnstratum

import (
	"context"
	"fmt"
	"time"

	"github.com/Hoosat-Oy/htn-stratum-bridge/src/gostratum"
	"github.com/Hoosat-Oy/htnd/app/appmessage"
	"github.com/Hoosat-Oy/htnd/infrastructure/network/rpcclient"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type HtnApi struct {
	address       string
	blockWaitTime time.Duration
	logger        *zap.SugaredLogger
	hoosat        *rpcclient.RPCClient
	connected     bool
}

func NewPyrinAPI(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*HtnApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &HtnApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "hoosatapi:"+address)),
		hoosat:        client,
		connected:     true,
	}, nil
}

func (py *HtnApi) Start(ctx context.Context, blockCb func()) {
	py.waitForSync(true)
	go py.startBlockTemplateListener(ctx, blockCb)
	go py.startStatsThread(ctx)
}

func (py *HtnApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			py.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := py.hoosat.GetBlockDAGInfo()
			if err != nil {
				py.logger.Warn("failed to get network hashrate from hoosat, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := py.hoosat.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				py.logger.Warn("failed to get network hashrate from hoosat, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (py *HtnApi) reconnect() error {
	if py.hoosat != nil {
		return py.hoosat.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(py.address)
	if err != nil {
		return err
	}
	py.hoosat = client
	return nil
}

func (s *HtnApi) waitForSync(verbose bool) error {
	if verbose {
		s.logger.Info("checking hoosat sync state")
	}
	for {
		clientInfo, err := s.hoosat.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from hoosat @ %s", s.address)
		}
		if clientInfo.IsSynced {
			break
		}
		s.logger.Warn("HTN is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		s.logger.Info("HTN synced, starting server")
	}
	return nil
}

func (s *HtnApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	blockReadyChan := make(chan bool)
	err := s.hoosat.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
		blockReadyChan <- true
	})
	if err != nil {
		s.logger.Error("fatal: failed to register for block notifications from hoosat")
	}

	ticker := time.NewTicker(s.blockWaitTime)
	for {
		if err := s.waitForSync(false); err != nil {
			s.logger.Error("error checking hoosat sync state, attempting reconnect: ", err)
			if err := s.reconnect(); err != nil {
				s.logger.Error("error reconnecting to hoosat, waiting before retry: ", err)
				time.Sleep(5 * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			s.logger.Warn("context cancelled, stopping block update listener")
			return
		case <-blockReadyChan:
			blockReadyCb()
			ticker.Reset(s.blockWaitTime)
		case <-ticker.C: // timeout, manually check for new blocks
			blockReadyCb()
		}
	}
}

func (py *HtnApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := py.hoosat.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via Pyrinpyi/hoosat-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from hoosat")
	}
	return template, nil
}
