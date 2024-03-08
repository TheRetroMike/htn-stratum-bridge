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
	htnd          *rpcclient.RPCClient
	connected     bool
}

func NewHtnAPI(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*HtnApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &HtnApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "htnapi:"+address)),
		htnd:          client,
		connected:     true,
	}, nil
}

func (api *HtnApi) Start(ctx context.Context, blockCb func()) {
	api.waitForSync(true)
	go api.startBlockTemplateListener(ctx, blockCb)
	go api.startStatsThread(ctx)
}

func (api *HtnApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			api.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := api.htnd.GetBlockDAGInfo()
			if err != nil {
				api.logger.Warn("failed to get network hashrate from htnd, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := api.htnd.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				api.logger.Warn("failed to get network hashrate from htnd, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (api *HtnApi) reconnect() error {
	if api.htnd != nil {
		return api.htnd.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(api.address)
	if err != nil {
		return err
	}
	api.htnd = client
	return nil
}

func (s *HtnApi) waitForSync(verbose bool) error {
	if verbose {
		s.logger.Info("checking htnd sync state")
	}
	for {
		clientInfo, err := s.htnd.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from htnd @ %s", s.address)
		}
		if clientInfo.IsSynced {
			break
		}
		s.logger.Warn("HTND is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		s.logger.Info("htnd synced, starting server")
	}
	return nil
}

func (s *HtnApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	blockReadyChan := make(chan bool)
	err := s.htnd.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
		blockReadyChan <- true
	})
	if err != nil {
		s.logger.Error("fatal: failed to register for block notifications from htnd")
	}

	ticker := time.NewTicker(s.blockWaitTime)
	for {
		if err := s.waitForSync(false); err != nil {
			s.logger.Error("error checking htnd sync state, attempting reconnect: ", err)
			if err := s.reconnect(); err != nil {
				s.logger.Error("error reconnecting to htnd, waiting before retry: ", err)
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

func (api *HtnApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := api.htnd.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via hoosat-oy/htnd-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from htnd")
	}
	return template, nil
}
