// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package haproxyreload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type candidate struct {
	phase       string
	transaction string
	hash        string
	config      []byte
	previous    []byte
}

const (
	// candidatePollActive applies while a transaction is in progress, when the next
	// phase is expected within seconds.
	candidatePollActive = time.Second
	// candidatePollIdle bounds how long a new transaction goes unnoticed.
	candidatePollIdle = 3 * time.Second
	// candidateFallbackRetry spaces API attempts while only the mounted copy is usable.
	candidateFallbackRetry = 5 * time.Second
)

// newCandidate builds a candidate from the ConfigMap keys and checks that its metadata
// is consistent, so a partially updated copy is never acted on.
func newCandidate(phase, transaction, hash string, config, previous []byte) (candidate, error) {
	c := candidate{
		phase:       strings.TrimSpace(phase),
		transaction: strings.TrimSpace(transaction),
		hash:        strings.TrimSpace(hash),
		config:      config,
		previous:    previous,
	}
	if c.transaction == "" || c.hash == "" || Hash(c.config) != c.hash {
		return candidate{}, errors.New("candidate configuration metadata is inconsistent")
	}
	return c, nil
}

// readCandidate reads the copy that the kubelet projects into the pod. It can lag
// behind the ConfigMap by a minute or more, so it is only a fallback.
func readCandidate(dir string) (candidate, error) {
	revision, err := filepath.EvalSymlinks(filepath.Join(dir, "..data"))
	if err != nil {
		return candidate{}, err
	}
	files := map[string][]byte{}
	for _, name := range []string{"phase", "transaction", "hash", "haproxy.cfg", "previous.cfg"} {
		data, err := os.ReadFile(filepath.Join(revision, name))
		if err != nil {
			return candidate{}, err
		}
		files[name] = data
	}
	return newCandidate(string(files["phase"]), string(files["transaction"]), string(files["hash"]),
		files["haproxy.cfg"], files["previous.cfg"])
}

// fetchCandidate reads the candidate ConfigMap through the Kubernetes API, which
// reflects the operator's latest write instead of waiting for the kubelet to refresh
// the projected volume.
func fetchCandidate(ctx context.Context, client *http.Client, endpoint, tokenPath string) (candidate, error) {
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return candidate{}, fmt.Errorf("reading projected Kubernetes token: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return candidate{}, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return candidate{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return candidate{}, fmt.Errorf("Kubernetes API returned %s: %s", response.Status, body)
	}
	var configMap struct {
		Data map[string]string `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&configMap); err != nil {
		return candidate{}, fmt.Errorf("decoding candidate ConfigMap: %w", err)
	}
	for _, key := range []string{"phase", "transaction", "hash", "haproxy.cfg", "previous.cfg"} {
		if _, present := configMap.Data[key]; !present {
			return candidate{}, fmt.Errorf("candidate ConfigMap has no %q key", key)
		}
	}
	d := configMap.Data
	return newCandidate(d["phase"], d["transaction"], d["hash"], []byte(d["haproxy.cfg"]), []byte(d["previous.cfg"]))
}

// candidateSource provides the current candidate. It asks the API at a pace that
// follows the transaction (fast while one is running, slower when idle) and falls
// back to the mounted copy when the API cannot be used, for example during an API
// server outage or before the Role that permits the read has been updated.
type candidateSource struct {
	client    *http.Client
	url       string
	tokenPath string
	dir       string
	now       func() time.Time

	cached     candidate
	haveCached bool
	nextFetch  time.Time
	lastAPIErr string
}

func (s *candidateSource) get(ctx context.Context) (candidate, error) {
	now := s.now()
	if s.haveCached && now.Before(s.nextFetch) {
		return s.cached, nil
	}
	c, err := fetchCandidate(ctx, s.client, s.url, s.tokenPath)
	if err == nil {
		s.lastAPIErr = ""
		s.cached, s.haveCached = c, true
		s.nextFetch = now.Add(candidatePollActive)
		if c.phase == "idle" {
			s.nextFetch = now.Add(candidatePollIdle)
		}
		return c, nil
	}
	if err.Error() != s.lastAPIErr {
		fmt.Fprintf(os.Stderr, "haproxy-reloader: reading candidate through the API (using the mounted copy): %v\n", err)
		s.lastAPIErr = err.Error()
	}
	s.nextFetch = now.Add(candidateFallbackRetry)
	c, mountErr := readCandidate(s.dir)
	if mountErr != nil {
		return candidate{}, fmt.Errorf("%w; mounted copy: %v", err, mountErr)
	}
	s.cached, s.haveCached = c, true
	return c, nil
}
