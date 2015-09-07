// Copyright 2015 bs authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package status

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/tsuru/bs/container"
)

type containerStatus struct {
	ID     string
	Name   string
	Status string
}

type respUnit struct {
	ID    string
	Found bool
}

type ReporterConfig struct {
	Interval       time.Duration
	DockerEndpoint string
	TsuruEndpoint  string
	TsuruToken     string
}

type Reporter struct {
	config     *ReporterConfig
	abort      chan<- struct{}
	exit       <-chan struct{}
	infoClient *container.InfoClient
	httpClient *http.Client
}

const (
	dialTimeout = 10 * time.Second
	fullTimeout = 1 * time.Minute
)

// NewReporter starts the status reporter. It will run intermitently, sending a
// message in the exit channel in case it exits. It's possible to arbitrarily
// interrupt the reporter by sending a message in the abort channel.
func NewReporter(config *ReporterConfig) (*Reporter, error) {
	abort := make(chan struct{})
	exit := make(chan struct{})
	infoClient, err := container.NewClient(config.DockerEndpoint)
	if err != nil {
		return nil, err
	}
	transport := http.Transport{
		Dial: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: dialTimeout,
	}
	reporter := Reporter{
		config:     config,
		abort:      abort,
		exit:       exit,
		infoClient: infoClient,
		httpClient: &http.Client{
			Transport: &transport,
			Timeout:   fullTimeout,
		},
	}
	go func(abort <-chan struct{}) {
		for {
			select {
			case <-abort:
				close(exit)
				return
			case <-time.After(reporter.config.Interval):
				reporter.reportStatus()
			}
		}
	}(abort)
	return &reporter, nil
}

// Stop stops the reporter. It will block until it actually stops (i.e. there's
// no need to call Wait after calling Stop).
func (r *Reporter) Stop() {
	close(r.abort)
	<-r.exit
}

// Wait blocks until the reporter stops.
func (r *Reporter) Wait() {
	<-r.exit
}

func (r *Reporter) reportStatus() {
	client := r.infoClient.GetClient()
	opts := docker.ListContainersOptions{All: true}
	containers, err := client.ListContainers(opts)
	if err != nil {
		log.Printf("[ERROR] failed to list containers in the Docker server at %q: %s", r.config.DockerEndpoint, err)
		return
	}
	resp, err := r.updateUnits(containers)
	if err != nil {
		log.Printf("[ERROR] failed to send data to the tsuru server at %q: %s", r.config.TsuruEndpoint, err)
		return
	}
	if len(resp) == 0 {
		return
	}
	err = r.handleTsuruResponse(resp)
	if err != nil {
		log.Printf("[ERROR] failed to handle tsuru response: %s", err)
	}
}

func (r *Reporter) updateUnits(containers []docker.APIContainers) ([]respUnit, error) {
	payload := make([]containerStatus, 0, len(containers))
	for _, c := range containers {
		var status string
		cont, err := r.infoClient.GetFreshContainer(c.ID)
		if err == container.ErrTsuruVariablesNotFound {
			continue
		}
		if err != nil {
			log.Printf("[ERROR] failed to inspect container %q: %s", c.ID, err)
			status = "error"
		} else {
			if cont.Container.State.Restarting {
				status = "error"
			} else if cont.Container.State.Running {
				status = "started"
			} else {
				status = "stopped"
			}
		}
		var name string
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		payload = append(payload, containerStatus{ID: c.ID, Name: name, Status: status})
	}
	if len(payload) == 0 {
		return nil, nil
	}
	var body bytes.Buffer
	err := json.NewEncoder(&body).Encode(payload)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/units/status", strings.TrimRight(r.config.TsuruEndpoint, "/"))
	request, err := http.NewRequest("POST", url, &body)
	if err != nil {
		return nil, err
	}
	request.Header.Add("Content-Type", "application/json")
	request.Header.Add("Authorization", "bearer "+r.config.TsuruToken)
	resp, err := r.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	var statusResp []respUnit
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&statusResp)
	if err != nil {
		return nil, err
	}
	return statusResp, nil
}

func (r *Reporter) handleTsuruResponse(resp []respUnit) error {
	goneUnits := make([]string, 0, len(resp))
	for _, unit := range resp {
		if !unit.Found {
			goneUnits = append(goneUnits, unit.ID)
		}
	}
	client := r.infoClient.GetClient()
	for _, id := range goneUnits {
		opts := docker.RemoveContainerOptions{ID: id, Force: true}
		err := client.RemoveContainer(opts)
		if err != nil {
			log.Printf("[ERROR] failed to remove container %q: %s", id, err)
		}
	}
	return nil
}