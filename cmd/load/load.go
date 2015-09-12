package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dt/go-metrics-reporting"
	"github.com/dt/thile/client"
	"github.com/dt/thile/gen"
)

type Load struct {
	collection string
	sample     *int64
	keys       [][]byte

	server string
	diff   *string

	work chan bool

	rtt       report.Timer
	diffRtt   report.Timer
	diffs     report.Meter
	dropped   report.Meter
	queueSize report.Guage

	sync.RWMutex
}

func (l *Load) setKeys() error {
	c := thttp.NewThriftHttpRpcClient(l.server)
	r := &gen.InfoRequest{&l.collection, l.sample}

	if resp, err := c.GetInfo(r); err != nil {
		return err
	} else {
		if len(resp) < 1 || len(resp[0].RandomKeys) < 1 {
			return fmt.Errorf("Response (len %d) contained no keys!", len(resp))
		}
		l.Lock()
		l.keys = resp[0].RandomKeys
		l.Unlock()
		return nil
	}
}

func (l *Load) startKeyFetcher(freq time.Duration) {
	for _ = range time.Tick(freq) {
		log.Println("Fetching new keys...")
		l.setKeys()
	}
}

func (l *Load) generator(qps int) {
	pause := time.Duration(time.Second.Nanoseconds() / int64(qps))

	for _ = range time.Tick(pause) {
		l.queueSize.Update(int64(len(l.work)))
		select {
		case l.work <- true:
		default:
			l.dropped.Mark(1)
		}
	}
}

func (l *Load) sendOne(client *gen.HFileServiceClient, diff *gen.HFileServiceClient) {
	// TODO: randomly change up request type generated
	l.sendSingle(client, diff)
}

func (l *Load) sendSingle(client *gen.HFileServiceClient, diff *gen.HFileServiceClient) {
	r := &gen.SingleHFileKeyRequest{HfileName: &l.collection, SortedKeys: l.randomKeys(1)}

	before := time.Now()
	resp, err := client.GetValuesSingle(r)
	if err != nil {
		log.Println("Error fetching value:", err)
	}
	l.rtt.UpdateSince(before)

	if diff != nil {
		beforeDiff := time.Now()
		diffResp, err := diff.GetValuesSingle(r)
		if err != nil {
			log.Println("Error fetching value:", err)
		}
		l.diffRtt.UpdateSince(beforeDiff)

		if !reflect.DeepEqual(resp, diffResp) {
			l.diffs.Mark(1)
			log.Printf("[DIFF] req: %v\n\torig (%d): %v\n\tdiff (%d): %v\n", r, resp.GetKeyCount(), resp, diffResp.GetKeyCount(), diffResp)
		}
	}
}

func (l *Load) randomKeys(count int) [][]byte {
	indexes := make([]int, count)
	l.RLock()
	for i := 0; i < count; i++ {
		indexes[i] = rand.Intn(len(l.keys))
	}
	sort.Ints(indexes)

	out := make([][]byte, count)
	for i := 0; i < count; i++ {
		out[i] = l.keys[indexes[i]]
	}
	l.RUnlock()
	return out
}

func (l *Load) startWorkers(count int) {
	for i := 0; i < count; i++ {
		go func() {
			client := thttp.NewThriftHttpRpcClient(l.server)
			var diff *gen.HFileServiceClient
			if l.diff != nil && len(*l.diff) > 0 {
				diff = thttp.NewThriftHttpRpcClient(*l.diff)
			}
			for {
				<-l.work
				l.sendOne(client, diff)
			}
		}()
	}
}

func hfileUrlAndName(s string) (string, string) {
	name := strings.NewReplacer("http://", "", ".", "_", ":", "_", "/", "_").Replace(s)

	if parts := strings.Split(s, "="); len(parts) > 1 {
		s = parts[1]
		name = parts[0]
	}

	if !strings.Contains(s, "/") {
		fmt.Println("URL doens't appear to specify a path -- appending /rpc/HFileService")
		s = s + "/rpc/HFileService"
	}

	if !strings.HasPrefix(s, "http") {
		s = "http://" + s
	}
	return s, name
}

func main() {
	orig := flag.String("server", "localhost:9999", "URL of hfile server")
	rawDiff := flag.String("diff", "", "URL of second hfile server to compare")
	collection := flag.String("collection", "testdata", "name of collection")
	graphite := report.Flag()
	workers := flag.Int("workers", 8, "worker pool size")
	qps := flag.Int("qps", 100, "qps to attempt")
	sample := flag.Int64("sampleSize", 1000, "number of random keys to use")

	flag.Parse()

	r := report.NewRecorder().
		MaybeReportTo(graphite).
		LogToConsole(time.Second * 10).
		SetAsDefault()

	rttName := "rtt"
	server, name := hfileUrlAndName(*orig)

	var diffRtt report.Timer
	var diffs report.Meter
	var diff *string
	if rawDiff != nil && len(*rawDiff) > 0 {
		diffServer, diffName := hfileUrlAndName(*rawDiff)
		diff = &diffServer
		diffRtt = r.GetTimer("rtt." + diffName)
		diffs = r.GetMeter("diffs")
		rttName = "rtt." + name
	}

	l := &Load{
		collection: *collection,
		sample:     sample,
		server:     server,
		diff:       diff,
		work:       make(chan bool, (*qps)*(*workers)),
		dropped:    r.GetMeter("dropped"),
		queueSize:  r.GetGuage("queue"),
		rtt:        r.GetTimer(rttName),
		diffRtt:    diffRtt,
		diffs:      diffs,
	}

	if err := l.setKeys(); err != nil {
		fmt.Println("Failed to fetch testing keys:", err)
		os.Exit(1)
	}

	fmt.Printf("Sending %dqps to %s, drawing from %d random keys...\n", *qps, server, len(l.keys))

	l.startWorkers(*workers)
	go l.startKeyFetcher(time.Minute)
	l.generator(*qps)

}
