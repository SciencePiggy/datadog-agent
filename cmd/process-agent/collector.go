package main

import (
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DataDog/datadog-agent/pkg/forwarder"

	"github.com/DataDog/datadog-agent/pkg/util/log"

	model "github.com/DataDog/agent-payload/process"
	"github.com/DataDog/datadog-agent/pkg/process/checks"
	"github.com/DataDog/datadog-agent/pkg/process/config"
	"github.com/DataDog/datadog-agent/pkg/process/statsd"
	"github.com/DataDog/datadog-agent/pkg/process/util"
	"github.com/DataDog/datadog-agent/pkg/process/util/api"
	"github.com/DataDog/datadog-agent/pkg/util/kubernetes/clustername"
)

type checkPayload struct {
	messages []model.MessageBody
	name     string
}

// Collector will collect metrics from the local system and ship to the backend.
type Collector struct {
	// Set to 1 if enabled 0 is not. We're using an integer
	// so we can use the sync/atomic for thread-safe access.
	realTimeEnabled int32

	groupID int32

	rtIntervalCh chan time.Duration
	cfg          *config.AgentConfig

	// counters for each type of check
	runCounters   sync.Map
	enabledChecks []checks.Check

	// Controls the real-time interval, can change live.
	realTimeInterval time.Duration
}

// NewCollector creates a new Collector
func NewCollector(cfg *config.AgentConfig) (Collector, error) {
	sysInfo, err := checks.CollectSystemInfo(cfg)
	if err != nil {
		return Collector{}, err
	}

	enabledChecks := make([]checks.Check, 0)
	for _, c := range checks.All {
		if cfg.CheckIsEnabled(c.Name()) {
			c.Init(cfg, sysInfo)
			enabledChecks = append(enabledChecks, c)
		}
	}

	return NewCollectorWithChecks(cfg, enabledChecks), nil
}

// NewCollectorWithChecks creates a new Collector
func NewCollectorWithChecks(cfg *config.AgentConfig, checks []checks.Check) Collector {
	return Collector{
		rtIntervalCh:  make(chan time.Duration),
		cfg:           cfg,
		groupID:       rand.Int31(),
		enabledChecks: checks,

		// Defaults for real-time on start
		realTimeInterval: 2 * time.Second,
		realTimeEnabled:  0,
	}
}

func (l *Collector) runCheck(c checks.Check, payloads chan checkPayload) {
	runCounter := int32(1)
	if rc, ok := l.runCounters.Load(c.Name()); ok {
		runCounter = rc.(int32) + 1
	}
	l.runCounters.Store(c.Name(), runCounter)

	s := time.Now()
	// update the last collected timestamp for info
	updateLastCollectTime(time.Now())
	messages, err := c.Run(l.cfg, atomic.AddInt32(&l.groupID, 1))
	if err != nil {
		log.Errorf("Unable to run check '%s': %s", c.Name(), err)
	} else {
		payloads <- checkPayload{messages, c.Name()}
		// update proc and container count for info
		updateProcContainerCount(messages)
		if !c.RealTime() {
			d := time.Since(s)
			switch {
			case runCounter < 5:
				log.Infof("Finished %s check #%d in %s", c.Name(), runCounter, d)
			case runCounter == 5:
				log.Infof("Finished %s check #%d in %s. First 5 check runs finished, next runs will be logged every 20 runs.", c.Name(), runCounter, d)
			case runCounter%20 == 0:
				log.Infof("Finish %s check #%d in %s", c.Name(), runCounter, d)
			}
		}
	}
}

func (l *Collector) run(exit chan bool) error {
	eps := make([]string, 0, len(l.cfg.APIEndpoints))
	for _, e := range l.cfg.APIEndpoints {
		eps = append(eps, e.Endpoint.String())
	}
	orchestratorEps := make([]string, 0, len(l.cfg.OrchestratorEndpoints))
	for _, e := range l.cfg.OrchestratorEndpoints {
		orchestratorEps = append(orchestratorEps, e.Endpoint.String())
	}
	log.Infof("Starting process-agent for host=%s, endpoints=%s, orchestrator endpoints=%s, enabled checks=%v", l.cfg.HostName, eps, orchestratorEps, l.cfg.EnabledChecks)

	go util.HandleSignals(exit)

	processPayloads := make(chan checkPayload, l.cfg.QueueSize)
	podPayloads := make(chan checkPayload, l.cfg.QueueSize)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()

		queueSizeTicker := time.NewTicker(10 * time.Second)
		defer queueSizeTicker.Stop()

		tags := []string{
			fmt.Sprintf("version:%s", Version),
			fmt.Sprintf("revision:%s", GitCommit),
		}
		for {
			select {
			case <-heartbeat.C:
				statsd.Client.Gauge("datadog.process.agent", 1, tags, 1)
			case <-queueSizeTicker.C:
				updateQueueSize(len(processPayloads), len(podPayloads))
			case <-exit:
				return
			}
		}
	}()

	processForwarderOpts := forwarder.NewOptions(keysPerDomains(l.cfg.APIEndpoints))
	processForwarderOpts.EnableHealthChecking = false
	processForwarder := forwarder.NewDefaultForwarder(processForwarderOpts)

	podForwarderOpts := forwarder.NewOptions(keysPerDomains(l.cfg.OrchestratorEndpoints))
	podForwarderOpts.EnableHealthChecking = false
	podForwarder := forwarder.NewDefaultForwarder(podForwarderOpts)

	if err := processForwarder.Start(); err != nil {
		return fmt.Errorf("error starting forwarder: %s", err)
	}

	if err := podForwarder.Start(); err != nil {
		return fmt.Errorf("error starting pod forwarder: %s", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		l.consumePayloads(processPayloads, processForwarder, exit)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		l.consumePayloads(podPayloads, podForwarder, exit)
	}()

	for _, c := range l.enabledChecks {
		payloads := processPayloads
		if c.Name() == checks.Pod.Name() {
			payloads = podPayloads
		}

		wg.Add(1)
		go func(c checks.Check, payloads chan checkPayload) {
			defer wg.Done()

			// Run the check the first time to prime the caches.
			if !c.RealTime() {
				l.runCheck(c, payloads)
			}

			ticker := time.NewTicker(l.cfg.CheckInterval(c.Name()))
			for {
				select {
				case <-ticker.C:
					realTimeEnabled := atomic.LoadInt32(&l.realTimeEnabled) == 1
					if !c.RealTime() || realTimeEnabled {
						l.runCheck(c, payloads)
					}
				case d := <-l.rtIntervalCh:
					// Live-update the ticker.
					if c.RealTime() {
						ticker.Stop()
						ticker = time.NewTicker(d)
					}
				case _, ok := <-exit:
					if !ok {
						return
					}
				}
			}
		}(c, payloads)
	}

	<-exit
	wg.Wait()

	processForwarder.Stop()
	podForwarder.Stop()
	return nil
}

func (l *Collector) consumePayloads(payloads chan checkPayload, fwd forwarder.Forwarder, exit chan bool) {
	for {
		select {
		case payload := <-payloads:
			if len(payloads) >= l.cfg.QueueSize {
				log.Info("Expiring payload from in-memory queue.")
				// Limit number of items kept in memory while we wait.
				<-payloads
			}

			for _, m := range payload.messages {
				extraHeaders := make(http.Header)
				extraHeaders.Set(api.HostHeader, l.cfg.HostName)
				extraHeaders.Set(api.ProcessVersionHeader, Version)
				extraHeaders.Set(api.ContainerCountHeader, strconv.Itoa(getContainerCount(m)))

				if cid, err := clustername.GetClusterID(); err == nil && cid != "" {
					extraHeaders.Set(api.ClusterIDHeader, cid)
				}

				body, err := encodePayload(m)
				if err != nil {
					log.Errorf("Unable to encode message: %s", err)
					continue
				}

				payloads := forwarder.Payloads{&body}
				var responses chan forwarder.Response

				switch payload.name {
				case checks.Process.Name():
					responses, err = fwd.SubmitProcessChecks(payloads, extraHeaders)
				case checks.RTProcess.Name():
					responses, err = fwd.SubmitRTProcessChecks(payloads, extraHeaders)
				case checks.Container.Name():
					responses, err = fwd.SubmitContainerChecks(payloads, extraHeaders)
				case checks.RTContainer.Name():
					responses, err = fwd.SubmitRTContainerChecks(payloads, extraHeaders)
				case checks.Connections.Name():
					responses, err = fwd.SubmitConnectionChecks(payloads, extraHeaders)
				case checks.Pod.Name():
					responses, err = fwd.SubmitPodChecks(payloads, extraHeaders)
				default:
					err = fmt.Errorf("unsupported payload type: %s", payload.name)
				}

				if err != nil {
					log.Errorf("Unable to submit payload: %s", err)
					continue
				}

				if statuses := readResponseStatuses(responses); len(statuses) > 0 {
					l.updateStatus(statuses)
				}
			}
		case <-exit:
			return
		}
	}
}

func (l *Collector) updateStatus(statuses []*model.CollectorStatus) {
	curEnabled := atomic.LoadInt32(&l.realTimeEnabled) == 1

	// If any of the endpoints wants real-time we'll do that.
	// We will pick the maximum interval given since generally this is
	// only set if we're trying to limit load on the backend.
	shouldEnableRT := false
	maxInterval := 0 * time.Second
	for _, s := range statuses {
		shouldEnableRT = shouldEnableRT || (s.ActiveClients > 0 && l.cfg.AllowRealTime)
		interval := time.Duration(s.Interval) * time.Second
		if interval > maxInterval {
			maxInterval = interval
		}
	}

	if curEnabled && !shouldEnableRT {
		log.Info("Detected 0 clients, disabling real-time mode")
		atomic.StoreInt32(&l.realTimeEnabled, 0)
	} else if !curEnabled && shouldEnableRT {
		log.Info("Detected active clients, enabling real-time mode")
		atomic.StoreInt32(&l.realTimeEnabled, 1)
	}

	if maxInterval != l.realTimeInterval {
		l.realTimeInterval = maxInterval
		if l.realTimeInterval <= 0 {
			l.realTimeInterval = 2 * time.Second
		}
		// Pass along the real-time interval, one per check, so that every
		// check routine will see the new interval.
		for range l.enabledChecks {
			l.rtIntervalCh <- l.realTimeInterval
		}
		log.Infof("real time interval updated to %s", l.realTimeInterval)
	}
}

// getContainerCount returns the number of containers in the message body
func getContainerCount(mb model.MessageBody) int {
	switch v := mb.(type) {
	case *model.CollectorProc:
		return len(v.GetContainers())
	case *model.CollectorRealTime:
		return len(v.GetContainerStats())
	case *model.CollectorContainer:
		return len(v.GetContainers())
	case *model.CollectorContainerRealTime:
		return len(v.GetStats())
	case *model.CollectorConnections:
		return 0
	}
	return 0
}

func readResponseStatuses(responses chan forwarder.Response) []*model.CollectorStatus {
	var statuses []*model.CollectorStatus

	for response := range responses {
		if response.Err != nil {
			log.Errorf("Error from %s: %s", response.Domain, response.Err)
			continue
		}

		if response.StatusCode >= 300 {
			log.Errorf("Invalid response from %s: %d -> %s", response.Domain, response.StatusCode, response.Err)
			continue
		}

		r, err := model.DecodeMessage(response.Body)
		if err != nil {
			log.Errorf("Could not decode response body: %s", err)
			continue
		}

		switch r.Header.Type {
		case model.TypeResCollector:
			rm := r.Body.(*model.ResCollector)
			if len(rm.Message) > 0 {
				log.Errorf("Error in response from %s: %s", response.Domain, rm.Message)
			} else {
				statuses = append(statuses, rm.Status)
			}
		default:
			log.Errorf("Unexpected response type from %s: %d", response.Domain, r.Header.Type)
		}
	}

	return statuses
}

func encodePayload(m model.MessageBody) ([]byte, error) {
	msgType, err := model.DetectMessageType(m)
	if err != nil {
		return nil, fmt.Errorf("unable to detect message type: %s", err)
	}

	return model.EncodeMessage(model.Message{
		Header: model.MessageHeader{
			Version:  model.MessageV3,
			Encoding: model.MessageEncodingZstdPB,
			Type:     msgType,
		}, Body: m})
}

func keysPerDomains(endpoints []api.Endpoint) map[string][]string {
	keysPerDomains := make(map[string][]string)

	for _, ep := range endpoints {
		keysPerDomains[removePathIfPresent(ep.Endpoint)] = []string{ep.APIKey}
	}

	return keysPerDomains
}

// removePathIfPresent removes the path component from the URL if it is present
func removePathIfPresent(url *url.URL) string {
	return fmt.Sprintf("%s://%s", url.Scheme, url.Host)
}
