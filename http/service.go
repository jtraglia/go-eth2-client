// Copyright © 2020 - 2024 Attestant Limited.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	client "github.com/attestantio/go-eth2-client"
	"github.com/attestantio/go-eth2-client/api"
	apiv1 "github.com/attestantio/go-eth2-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/rs/zerolog"
	zerologger "github.com/rs/zerolog/log"
	"golang.org/x/sync/semaphore"
)

// Service is an Ethereum 2 client service.
type Service struct {
	// log is a service-wide logger.
	log zerolog.Logger

	base    *url.URL
	address string
	client  *http.Client
	timeout time.Duration

	// Various information from the node that does not change during the
	// lifetime of a beacon node.
	genesis              *apiv1.Genesis
	genesisMutex         sync.RWMutex
	spec                 map[string]interface{}
	specMutex            sync.RWMutex
	depositContract      *apiv1.DepositContract
	depositContractMutex sync.RWMutex
	forkSchedule         []*phase0.Fork
	forkScheduleMutex    sync.RWMutex
	nodeVersion          string
	nodeVersionMutex     sync.RWMutex

	// User-specified chunk sizes.
	userIndexChunkSize  int
	userPubKeyChunkSize int
	extraHeaders        map[string]string

	// Endpoint support.
	pingSem                  *semaphore.Weighted
	connectionMu             sync.RWMutex
	connectionActive         bool
	connectionSynced         bool
	enforceJSON              bool
	connectedToDVTMiddleware bool
}

// New creates a new Ethereum 2 client service, connecting with a standard HTTP.
func New(ctx context.Context, params ...Parameter) (client.Service, error) {
	parameters, err := parseAndCheckParameters(params...)
	if err != nil {
		return nil, errors.Join(errors.New("problem with parameters"), err)
	}

	// Set logging.
	log := zerologger.With().Str("service", "client").Str("impl", "http").Logger()
	if parameters.logLevel != log.GetLevel() {
		log = log.Level(parameters.logLevel)
	}

	if parameters.monitor != nil {
		if err := registerMetrics(ctx, parameters.monitor); err != nil {
			return nil, errors.Join(errors.New("failed to register metrics"), err)
		}
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   parameters.timeout,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:        64,
			MaxConnsPerHost:     64,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     600 * time.Second,
		},
	}

	address := parameters.address
	if !strings.HasPrefix(address, "http") {
		address = fmt.Sprintf("http://%s", address)
	}
	if !strings.HasSuffix(address, "/") {
		address = fmt.Sprintf("%s/", address)
	}
	base, err := url.Parse(address)
	if err != nil {
		return nil, errors.Join(errors.New("invalid URL"), err)
	}

	s := &Service{
		log:                 log,
		base:                base,
		address:             parameters.address,
		client:              httpClient,
		timeout:             parameters.timeout,
		userIndexChunkSize:  parameters.indexChunkSize,
		userPubKeyChunkSize: parameters.pubKeyChunkSize,
		extraHeaders:        parameters.extraHeaders,
		enforceJSON:         parameters.enforceJSON,
		pingSem:             semaphore.NewWeighted(1),
	}

	// Ping the client to see if it is ready to serve requests.
	s.ping(ctx)
	active := s.IsActive(ctx)

	if !active && !parameters.allowDelayedStart {
		return nil, client.ErrNotActive
	}

	// Periodically refetch static values in case of client update.
	s.periodicClearStaticValues(ctx)

	// Periodically ping the client for state updates.
	s.periodicPing(ctx)

	// Close the service on context done.
	go func(s *Service) {
		<-ctx.Done()
		log.Trace().Msg("Context done; closing connection")
		s.close()
	}(s)

	return s, nil
}

// periodicPing periodically pings the client to update its active and synced status.
func (s *Service) periodicPing(ctx context.Context) {
	go func(s *Service, ctx context.Context) {
		// Refresh every 30 seconds.
		refreshTicker := time.NewTicker(30 * time.Second)
		defer refreshTicker.Stop()
		for {
			select {
			case <-refreshTicker.C:
				s.ping(ctx)
			case <-ctx.Done():
				return
			}
		}
	}(s, ctx)
}

// periodicClearStaticValues periodically sets static values to nil so they are
// refetched the next time they are required.
func (s *Service) periodicClearStaticValues(ctx context.Context) {
	go func(s *Service, ctx context.Context) {
		// Refresh every 5 minutes.
		refreshTicker := time.NewTicker(5 * time.Minute)
		defer refreshTicker.Stop()
		for {
			select {
			case <-refreshTicker.C:
				s.clearStaticValues()
			case <-ctx.Done():
				return
			}
		}
	}(s, ctx)
}

// clearStaticValues periodically sets static values to nil so they are
// refetched the next time they are required.
func (s *Service) clearStaticValues() {
	s.genesisMutex.Lock()
	s.genesis = nil
	s.genesisMutex.Unlock()
	s.specMutex.Lock()
	s.spec = nil
	s.specMutex.Unlock()
	s.depositContractMutex.Lock()
	s.depositContract = nil
	s.depositContractMutex.Unlock()
	s.forkScheduleMutex.Lock()
	s.forkSchedule = nil
	s.forkScheduleMutex.Unlock()
	s.nodeVersionMutex.Lock()
	s.nodeVersion = ""
	s.nodeVersionMutex.Unlock()
}

// checkDVT checks if connected to DVT middleware and sets
// internal flags appropriately.
func (s *Service) checkDVT(ctx context.Context) error {
	response, err := s.NodeVersion(ctx, &api.NodeVersionOpts{})
	if err != nil {
		return errors.Join(errors.New("failed to obtain node version for DVT check"), err)
	}

	version := strings.ToLower(response.Data)

	if strings.Contains(version, "charon") {
		s.connectedToDVTMiddleware = true
	}

	return nil
}

// Name provides the name of the service.
func (s *Service) Name() string {
	return "Standard (HTTP)"
}

// Address provides the address for the connection.
func (s *Service) Address() string {
	return s.address
}

// close closes the service, freeing up resources.
func (s *Service) close() {
}

// ping pings a client, potentially updating its activation and sync states.
func (s *Service) ping(_ context.Context) {
	// We ignore the context passed in, and create a separate context to ensure that
	// the ping isn't cancelled by a failed request that then subsequently calls here.
	ctx := context.Background()

	//nolint:contextcheck
	log := zerolog.Ctx(ctx)

	s.connectionMu.Lock()
	wasActive := s.connectionActive
	wasSynced := s.connectionSynced
	s.connectionMu.Unlock()

	var active bool
	var synced bool

	acquired := s.pingSem.TryAcquire(1)
	if !acquired {
		// Means there is another ping running, just use current info.
		active = wasActive
		synced = wasSynced
	} else {
		//nolint:contextcheck
		response, err := s.NodeSyncing(ctx, &api.NodeSyncingOpts{})
		if err != nil {
			log.Debug().Err(err).Msg("Failed to obtain sync state from node")
			active = false
			synced = false
		} else {
			active = true
			synced = (!response.Data.IsSyncing) || (response.Data.HeadSlot == 0 && response.Data.SyncDistance <= 1)
		}
		s.pingSem.Release(1)
	}

	switch {
	case !wasActive && active:
		// Switched from not active to active.

		// Check connection to DVT middleware.
		//nolint:contextcheck
		if err := s.checkDVT(ctx); err != nil {
			log.Error().Err(err).Msg("Failed to check DVT connection on client activation; returning to inactive")
			active = false
		}
	case wasActive && !active:
		// Switched from active to not active.

		// Clear static values.
		s.clearStaticValues()
	case !wasSynced && synced:
		// Switched from not synced to synced.
	case wasSynced && !synced:
		// Switched from synced to not synced.
	}

	log.Trace().Bool("was_active", wasActive).Bool("active", active).Bool("was_synced", wasSynced).Bool("synced", synced).Msg("Updated connection state")

	s.connectionMu.Lock()
	s.connectionActive = active
	s.connectionSynced = synced
	s.connectionMu.Unlock()

	s.monitorActive(active)
	s.monitorSynced(synced)
}

// IsActive returns true if the client is active.
func (s *Service) IsActive(_ context.Context) bool {
	s.connectionMu.RLock()
	active := s.connectionActive
	s.connectionMu.RUnlock()

	return active
}

// IsSynced returns true if the client is synced.
func (s *Service) IsSynced(_ context.Context) bool {
	s.connectionMu.RLock()
	synced := s.connectionSynced
	s.connectionMu.RUnlock()

	return synced
}

func (s *Service) assertIsActive(ctx context.Context) error {
	active := s.IsActive(ctx)
	if active {
		return nil
	}

	s.ping(ctx)
	active = s.IsActive(ctx)
	if !active {
		return client.ErrNotActive
	}

	return nil
}

func (s *Service) assertIsSynced(ctx context.Context) error {
	synced := s.IsSynced(ctx)
	if synced {
		return nil
	}

	s.ping(ctx)
	active := s.IsActive(ctx)
	if !active {
		return client.ErrNotActive
	}

	synced = s.IsSynced(ctx)
	if !synced {
		return client.ErrNotSynced
	}

	return nil
}
