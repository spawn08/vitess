/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
Package consultopo implements topo.Server with consul as the backend.
*/
package consultopo

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/spf13/pflag"

	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/servenv"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vterrors"
)

var (
	consulAuthClientStaticFile string
	// serfHealth is the default check from consul
	consulLockSessionChecks = "serfHealth"
	consulLockSessionTTL    string
	consulLockDelay         = 15 * time.Second
)

func init() {
	for _, cmd := range []string{"vtbackup", "vtcombo", "vtctl", "vtctld", "vtgate", "vtgr", "vttablet", "vttestserver", "zk"} {
		servenv.OnParseFor(cmd, registerServerFlags)
	}
}

func registerServerFlags(fs *pflag.FlagSet) {
	fs.StringVar(&consulAuthClientStaticFile, "consul_auth_static_file", consulAuthClientStaticFile, "JSON File to read the topos/tokens from.")
	fs.StringVar(&consulLockSessionChecks, "topo_consul_lock_session_checks", consulLockSessionChecks, "List of checks for consul session.")
	fs.StringVar(&consulLockSessionTTL, "topo_consul_lock_session_ttl", consulLockSessionTTL, "TTL for consul session.")
	fs.DurationVar(&consulLockDelay, "topo_consul_lock_delay", consulLockDelay, "LockDelay for consul session.")
}

// ClientAuthCred credential to use for consul clusters
type ClientAuthCred struct {
	// ACLToken when provided, the client will use this token when making requests to the Consul server.
	ACLToken string `json:"acl_token,omitempty"`
}

// Factory is the consul topo.Factory implementation.
type Factory struct{}

// HasGlobalReadOnlyCell is part of the topo.Factory interface.
func (f Factory) HasGlobalReadOnlyCell(serverAddr, root string) bool {
	return false
}

// Create is part of the topo.Factory interface.
func (f Factory) Create(cell, serverAddr, root string) (topo.Conn, error) {
	return NewServer(cell, serverAddr, root)
}

func getClientCreds() (creds map[string]*ClientAuthCred, err error) {
	creds = make(map[string]*ClientAuthCred)

	if consulAuthClientStaticFile == "" {
		// Not configured, nothing to do.
		log.Infof("Consul client auth is not set up. consul_auth_static_file was not provided")
		return nil, nil
	}

	data, err := os.ReadFile(consulAuthClientStaticFile)
	if err != nil {
		err = vterrors.Wrapf(err, "Failed to read consul_auth_static_file file")
		return creds, err
	}

	if err := json.Unmarshal(data, &creds); err != nil {
		err = vterrors.Wrapf(err, fmt.Sprintf("Error parsing consul_auth_static_file")) //nolint
		return creds, err
	}
	return creds, nil
}

// Server is the implementation of topo.Server for consul.
type Server struct {
	// client is the consul api client.
	client *api.Client
	kv     *api.KV

	// root is the root path for this client.
	root string

	// mu protects the following fields.
	mu sync.Mutex
	// locks is a map of *lockInstance structures.
	// The key is the filepath of the Lock file.
	locks map[string]*lockInstance

	lockChecks []string
	lockTTL    string
	lockDelay  time.Duration
}

// lockInstance keeps track of one lock held by this client.
type lockInstance struct {
	// lock has the api.Lock structure.
	lock *api.Lock

	// done is closed when the lock is release by this process.
	done chan struct{}
}

// NewServer returns a new consultopo.Server.
func NewServer(cell, serverAddr, root string) (*Server, error) {
	creds, err := getClientCreds()
	if err != nil {
		return nil, err
	}
	cfg := api.DefaultConfig()
	cfg.Address = serverAddr
	if creds != nil {
		if creds[cell] != nil {
			cfg.Token = creds[cell].ACLToken
		} else {
			log.Warningf("Client auth not configured for cell: %v", cell)
		}
	}

	client, err := api.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	return &Server{
		client:     client,
		kv:         client.KV(),
		root:       root,
		locks:      make(map[string]*lockInstance),
		lockChecks: parseConsulLockSessionChecks(consulLockSessionChecks),
		lockTTL:    consulLockSessionTTL,
		lockDelay:  consulLockDelay,
	}, nil
}

func parseConsulLockSessionChecks(s string) []string {
	var res []string
	if len(s) == 0 {
		return res
	}
	return strings.Split(consulLockSessionChecks, ",")
}

// Close implements topo.Server.Close.
// It will nil out the global and cells fields, so any attempt to
// re-use this server will panic.
func (s *Server) Close() {
	s.client = nil
	s.kv = nil
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locks = nil
}

func init() {
	topo.RegisterFactory("consul", Factory{})
}
