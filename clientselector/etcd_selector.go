package clientselector

import (
	"errors"
	"math/rand"
	"net"
	"net/rpc"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/context"

	"github.com/coreos/etcd/client"
	"github.com/smallnest/rpcx"
)

// EtcdClientSelector is used to select a rpc server from etcd.
type EtcdClientSelector struct {
	EtcdServers        []string
	KeysAPI            client.KeysAPI
	ticker             *time.Ticker
	sessionTimeout     time.Duration
	BasePath           string //should endwith serviceName
	Servers            []string
	WeightedServers    []*Weighted
	SelectMode         rpcx.SelectMode
	dailTimeout        time.Duration
	rnd                *rand.Rand
	currentServer      int
	len                int
	HashServiceAndArgs HashServiceAndArgs
	Client             *rpcx.Client
}

// NewEtcdClientSelector creates a EtcdClientSelector
func NewEtcdClientSelector(etcdServers []string, basePath string, sessionTimeout time.Duration, sm rpcx.SelectMode, dailTimeout time.Duration) *EtcdClientSelector {
	selector := &EtcdClientSelector{
		EtcdServers:    etcdServers,
		BasePath:       basePath,
		sessionTimeout: sessionTimeout,
		SelectMode:     sm,
		dailTimeout:    dailTimeout,
		rnd:            rand.New(rand.NewSource(time.Now().UnixNano()))}

	selector.start()
	return selector
}

//SetClient set a Client in order that clientSelector can uses it
func (s *EtcdClientSelector) SetClient(c *rpcx.Client) {
	s.Client = c
}

//SetSelectMode sets SelectMode
func (s *EtcdClientSelector) SetSelectMode(sm rpcx.SelectMode) {
	s.SelectMode = sm
}

//AllClients returns rpc.Clients to all servers
func (s *EtcdClientSelector) AllClients(clientCodecFunc rpcx.ClientCodecFunc) []*rpc.Client {
	var clients []*rpc.Client

	for _, sv := range s.Servers {
		ss := strings.Split(sv, "@")
		c, err := rpcx.NewDirectRPCClient(s.Client, clientCodecFunc, ss[0], ss[1], s.dailTimeout)
		if err == nil {
			clients = append(clients, c)
		}
	}

	return clients
}

func (s *EtcdClientSelector) start() {
	cli, err := client.New(client.Config{
		Endpoints:               s.EtcdServers,
		Transport:               client.DefaultTransport,
		HeaderTimeoutPerRequest: s.sessionTimeout,
	})

	if err != nil {
		return
	}
	s.KeysAPI = client.NewKeysAPI(cli)
	s.pullServers()

	// s.ticker = time.NewTicker(s.sessionTimeout)
	// go func() {
	// 	for range s.ticker.C {
	// 		s.pullServers()
	// 	}
	// }()

	go s.watch()
}

func (s *EtcdClientSelector) watch() {
	watcher := s.KeysAPI.Watcher(s.BasePath, &client.WatcherOptions{
		Recursive: true,
	})

	for {
		res, err := watcher.Next(context.Background())
		if err != nil {
			break
		}

		//services are changed, we pull service again instead of processing single node
		if res.Action == "expire" {
			s.pullServers()
		} else if res.Action == "set" || res.Action == "update" {
			s.pullServers()
		} else if res.Action == "delete" {
			s.pullServers()
		}
	}
}

func (s *EtcdClientSelector) pullServers() {
	resp, err := s.KeysAPI.Get(context.TODO(), s.BasePath, &client.GetOptions{
		Recursive: true,
		Sort:      true,
	})

	if err == nil && resp.Node != nil {
		if len(resp.Node.Nodes) > 0 {
			var servers []string
			for _, n := range resp.Node.Nodes {
				servers = append(servers, strings.TrimPrefix(n.Key, s.BasePath+"/"))

			}
			s.Servers = servers

			s.createWeighted(resp.Node.Nodes)

			//set weight based on ICMP result
			if s.SelectMode == rpcx.WeightedICMP {
				for _, w := range s.WeightedServers {
					server := w.Server.(string)
					ss := strings.Split(server, "@")
					host, _, _ := net.SplitHostPort(ss[1])
					rtt, _ := Ping(host)
					rtt = CalculateWeight(rtt)
					w.Weight = rtt
					w.EffectiveWeight = rtt
				}
			}

			s.len = len(s.Servers)
			s.currentServer = s.currentServer % s.len
		}

	}
}

func (s *EtcdClientSelector) createWeighted(nodes client.Nodes) {
	s.WeightedServers = make([]*Weighted, len(s.Servers))

	var inactiveServers []int

	for i, n := range nodes {
		s.WeightedServers[i] = &Weighted{Server: strings.TrimPrefix(n.Key, s.BasePath+"/"), Weight: 1, EffectiveWeight: 1}
		if v, err := url.ParseQuery(n.Value); err == nil {
			w := v.Get("weight")
			state := v.Get("state")
			if state != "" && state != "active" {
				inactiveServers = append(inactiveServers, i)
			}

			if w != "" {
				weight, err := strconv.Atoi(w)
				if err != nil {
					s.WeightedServers[i].Weight = weight
					s.WeightedServers[i].EffectiveWeight = weight
				}
			}
		}
	}

	s.removeInactiveServers(inactiveServers)
}

func (s *EtcdClientSelector) removeInactiveServers(inactiveServers []int) {
	i := len(inactiveServers) - 1
	for ; i >= 0; i-- {
		k := inactiveServers[i]
		s.Servers = append(s.Servers[0:k], s.Servers[k+1:]...)
		s.WeightedServers = append(s.WeightedServers[0:k], s.WeightedServers[k+1:]...)
	}
}

//Select returns a rpc client
func (s *EtcdClientSelector) Select(clientCodecFunc rpcx.ClientCodecFunc, options ...interface{}) (*rpc.Client, error) {
	if s.len == 0 {
		return nil, errors.New("No available service")
	}
	if s.SelectMode == rpcx.RandomSelect {
		s.currentServer = s.rnd.Intn(s.len)
		server := s.Servers[s.currentServer]
		ss := strings.Split(server, "@") //tcp@ip , tcp4@ip or tcp6@ip
		return rpcx.NewDirectRPCClient(s.Client, clientCodecFunc, ss[0], ss[1], s.dailTimeout)

	} else if s.SelectMode == rpcx.RoundRobin {
		s.currentServer = (s.currentServer + 1) % s.len //not use lock for performance so it is not precise even
		server := s.Servers[s.currentServer]
		ss := strings.Split(server, "@") //
		return rpcx.NewDirectRPCClient(s.Client, clientCodecFunc, ss[0], ss[1], s.dailTimeout)

	} else if s.SelectMode == rpcx.ConsistentHash {
		if s.HashServiceAndArgs == nil {
			s.HashServiceAndArgs = JumpConsistentHash
		}
		s.currentServer = s.HashServiceAndArgs(s.len, options)
		server := s.Servers[s.currentServer]
		ss := strings.Split(server, "@") //
		return rpcx.NewDirectRPCClient(s.Client, clientCodecFunc, ss[0], ss[1], s.dailTimeout)
	} else if s.SelectMode == rpcx.WeightedRoundRobin || s.SelectMode == rpcx.WeightedICMP {
		server := nextWeighted(s.WeightedServers).Server.(string)
		ss := strings.Split(server, "@")
		return rpcx.NewDirectRPCClient(s.Client, clientCodecFunc, ss[0], ss[1], s.dailTimeout)
	}

	return nil, errors.New("not supported SelectMode: " + s.SelectMode.String())

}
