package main

import (
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"

	"context"
	"io/ioutil"

	"github.com/stripe/sequins/blocks"
	pb "github.com/stripe/sequins/rpc"
	"sync"
)

// serveKey is the entrypoint for incoming HTTP requests. It looks up the value
// locally, for, failing that, asks a peer that has it. If the request was
// already proxied to us, it is not proxied further.
func (vs *version) serveKey(w http.ResponseWriter, r *http.Request, key string) {
	// If we don't have any data for this version at all, that's a 404.
	if vs.numPartitions == 0 {
		vs.serveNotFound(w)
		return
	}

	partition, alternatePartition := blocks.KeyPartition([]byte(key), vs.numPartitions)
	if vs.partitions.HaveLocal(partition) || vs.partitions.HaveLocal(alternatePartition) {
		record, err := vs.blockStore.Get(key)
		if err != nil {
			vs.serveError(w, key, err)
			return
		}

		vs.serveLocal(w, key, record)
	} else if r.URL.Query().Get("proxy") == "" {
		vs.serveProxied(w, r, key, partition, alternatePartition)
	} else {
		vs.serveError(w, key, errProxiedIncorrectly)
	}
}

func (vs *version) serveLocal(w http.ResponseWriter, key string, record *blocks.Record) {
	if record == nil {
		vs.serveNotFound(w)
		return
	}

	defer record.Close()
	w.Header().Set(versionHeader, vs.name)
	w.Header().Set("Content-Length", strconv.FormatUint(record.ValueLen, 10))
	w.Header().Set("Last-Modified", vs.created.UTC().Format(http.TimeFormat))
	_, err := io.Copy(w, record)
	if err != nil {
		// We already wrote a 200 OK, so not much we can do here except log.
		log.Printf("Error streaming response for /%s/%s (version %s): %s", vs.db.name, key, vs.name, err)
	}
}

func (vs *version) serveProxied(w http.ResponseWriter, r *http.Request,
	key string, partition, alternatePartition int) {

	// Shuffle the peers, so we try them in a random order.
	// TODO: We don't want to blacklist nodes, but we can weight them lower
	peers := shuffle(vs.partitions.FindPeers(partition))
	if len(peers) == 0 {
		log.Printf("No peers available for /%s/%s (version %s)", vs.db.name, key, vs.name)
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	resp, peer, err := vs.proxy(r, peers)
	if err == nil && resp.StatusCode == 404 && alternatePartition != partition {
		log.Println("Trying alternate partition for pathological key", key)

		resp.Body.Close()
		alternatePeers := shuffle(vs.partitions.FindPeers(alternatePartition))
		resp, peer, err = vs.proxy(r, alternatePeers)
	}

	if err == errNoAvailablePeers {
		// Either something is wrong with sharding, or all peers errored for some
		// other reason. 502
		log.Printf("No peers available for /%s/%s (version %s)", vs.db.name, key, vs.name)
		w.WriteHeader(http.StatusBadGateway)
		return
	} else if err == errProxyTimeout {
		// All of our peers failed us. 504.
		log.Printf("All peers timed out for /%s/%s (version %s)", vs.db.name, key, vs.name)
		w.WriteHeader(http.StatusGatewayTimeout)
		return
	} else if err != nil {
		// Some other error. 500.
		vs.serveError(w, key, err)
		return
	}

	// Proxying can produce inconsistent versions if something is broken. Use the
	// one the peer set.
	w.Header().Set(versionHeader, resp.Header.Get(versionHeader))
	w.Header().Set(proxyHeader, peer)
	w.Header().Set("Content-Length", resp.Header.Get("Content-Length"))
	w.Header().Set("Last-Modified", vs.created.UTC().Format(http.TimeFormat))
	w.WriteHeader(resp.StatusCode)

	// TODO: Apparently in 1.7 the client always asks for gzip by default. If our
	// client asks for gzip too, we should be able to pass through without
	// decompressing.
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		// We already wrote a 200 OK, so not much we can do here except log.
		log.Printf("Error copying response from peer for /%s/%s (version %s): %s", vs.db.name, key, vs.name, err)
	}
}

func (vs *version) serveNotFound(w http.ResponseWriter) {
	w.Header().Set(versionHeader, vs.name)
	w.WriteHeader(http.StatusNotFound)
}

func (vs *version) serveError(w http.ResponseWriter, key string, err error) {
	log.Printf("Error fetching value for /%s/%s: %s\n", vs.db.name, key, err)
	w.WriteHeader(http.StatusInternalServerError)
}

func shuffle(vs []string) []string {
	shuffled := make([]string, len(vs))
	perm := rand.Perm(len(vs))
	for i, v := range perm {
		shuffled[v] = vs[i]
	}

	return shuffled
}

// GRPC methods

func shuffleRPC(vs []pb.SequinsRpcClient) []pb.SequinsRpcClient {
	shuffled := make([]pb.SequinsRpcClient, len(vs))
	perm := rand.Perm(len(vs))
	for i, v := range perm {
		shuffled[v] = vs[i]
	}

	return shuffled
}

func (vs *version) GetKey(ctx context.Context, keyPb *pb.Key) (*pb.Record, error) {
	if vs.numPartitions == 0 {
		return nil, errNoAvailablePeers
	}
	key := string(keyPb.Key)
	partition, alternatePartition := blocks.KeyPartition([]byte(key), vs.numPartitions)
	if vs.partitions.HaveLocal(partition) || vs.partitions.HaveLocal(alternatePartition) {
		record, err := vs.blockStore.Get(key)
		if err != nil {
			return nil, err
		}
		value, err := ioutil.ReadAll(record)
		if err != nil {
			return nil, err
		}
		return &pb.Record{
			Key:     keyPb.Key,
			Value:   value,
			Version: vs.name,
			Proxied: false,
		}, nil

	} else if keyPb.ProxiedVersion != "" {
		vs.serveProxiedRPC(ctx, keyPb, partition, alternatePartition)
	}
	return nil, errNoAvailablePeers

}

func (vs *version) GetRange(ctx context.Context, rng *pb.Range, responseChan chan *pb.Record) error {
	if vs.numPartitions == 0 {
		return errNoAvailablePeers
	}

	startKey := string(rng.StartKey)
	endKey := string(rng.EndKey)

	startPartition, alternateStartPartition := blocks.KeyPartition(rng.StartKey, vs.numPartitions)
	endPartition, alternateEndPartition := blocks.KeyPartition(rng.EndKey, vs.numPartitions)
	// Check if we have to fan out.
	log.Println("vs GetRange", rng)
	log.Println("vs GetRange Start", startPartition, alternateStartPartition)
	log.Println("vs GetRange End", endPartition, alternateEndPartition)

	if startPartition == endPartition || startPartition == alternateEndPartition || alternateStartPartition == endPartition || alternateStartPartition == alternateEndPartition {
		log.Println("No fanout")
		if vs.partitions.HaveLocal(startPartition) || vs.partitions.HaveLocal(alternateStartPartition) {

			return vs.blockStore.GetRange(ctx, startKey, endKey, responseChan)
		}

	} else {
		log.Println("fanout")
		s := startPartition
		e := endPartition
		if  endPartition < startPartition {
			s = endPartition
			e = startPartition
		}
		log.Println(s,e)
		wg := sync.WaitGroup{}
		for i := s; i <= e; i++ {
			if vs.partitions.HaveLocal(i) {
				wg.Add(1)
				go vs.blockStore.GetRange(ctx, startKey, endKey, responseChan)
			} // else proxy
		}
		wg.Wait()
		return errNoAvailablePeers
		// fan out.
	}

	return nil
}

func (vs *version) serveProxiedRPC(ctx context.Context, keyPb *pb.Key, partition, alternatePartition int) (*pb.Record, error) {
	// Set key string.
	key := string(keyPb.Key)

	// Shuffle the peers, so we try them in a random order.
	// TODO: We don't want to blacklist nodes, but we can weight them lower
	peers := shuffleRPC(vs.partitions.FindGRPCPeers(partition))
	if len(peers) == 0 {
		log.Printf("No peers available for /%s/%s (version %s)", vs.db.name, key, vs.name)
		return nil, errNoAvailablePeers
	}

	keyPb.ProxiedVersion = vs.name

	//TODO: Timeout Logic.

	for _, peer := range peers {
		record, err := peer.GetKey(context.Background(), keyPb)
		if err == nil {
			return record, nil
		}
		record.Proxied = true
		// TODO Set proxied To
		//	record.ProxiedTo =

	}
	return nil, errNoAvailablePeers

}
