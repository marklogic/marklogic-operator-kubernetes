// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package backup

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/marklogic/marklogic-operator-kubernetes/pkg/objectstorage"
)

const liveGate = "INTEGRATION_BACKUP_S3"
const clusterName = "backup-server"
const clientName = "backup-client"
const credentialSecret = "backup-s3-credentials"

// Restrict destinations to ordinary AWS bucket names and an explicit test prefix.
// Never accept credentials, signed URLs, bucket roots, or path traversal here.
var bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
var prefixPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)

type config struct {
	BaseURI string
	Image   string
	AWS     objectstorage.AWSMaterial
}

func configFromEnvironment(getenv func(string) string) (config, error) {
	c := config{
		BaseURI: strings.TrimSpace(getenv("INTEGRATION_BACKUP_S3_URI")),
		Image:   strings.TrimSpace(getenv("MARKLOGIC_IMAGE")),
		// Match the operator's Secret normalization when checking the applied fingerprint.
		AWS: objectstorage.AWSMaterial{
			AccessKey:    strings.TrimSpace(getenv("AWS_ACCESS_KEY_ID")),
			SecretKey:    strings.TrimSpace(getenv("AWS_SECRET_ACCESS_KEY")),
			SessionToken: strings.TrimSpace(getenv("AWS_SESSION_TOKEN")),
		},
	}
	for _, key := range []string{"INTEGRATION_CONTEXT", "INTEGRATION_OPERATOR_NAMESPACE", "INTEGRATION_OPERATOR_DEPLOYMENT", "MARKLOGIC_IMAGE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if strings.TrimSpace(getenv(key)) == "" {
			return config{}, fmt.Errorf("set %s", key)
		}
	}
	parsed, err := url.Parse(c.BaseURI)
	if err != nil || parsed.Scheme != "s3" || !bucketPattern.MatchString(parsed.Host) || parsed.User != nil || strings.ContainsAny(c.BaseURI, "?#") || parsed.RawPath != "" || !prefixPattern.MatchString(strings.Trim(parsed.Path, "/")) || strings.Contains(parsed.Path, "//") {
		return config{}, fmt.Errorf("INTEGRATION_BACKUP_S3_URI must be s3://bucket/dedicated-test-prefix with plain alphanumeric, underscore, or hyphen path segments")
	}
	c.BaseURI = strings.TrimRight(c.BaseURI, "/")
	return c, nil
}

func backupPathWithinRun(candidate, runURI string) bool {
	if candidate == "" {
		return false
	}
	parsed, err := url.Parse(candidate)
	if err != nil || strings.ContainsAny(candidate, "?#") || parsed.User != nil || parsed.RawPath != "" {
		return false
	}
	if candidate != runURI && !strings.HasPrefix(candidate, runURI+"/") {
		return false
	}
	for _, part := range strings.Split(parsed.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}
