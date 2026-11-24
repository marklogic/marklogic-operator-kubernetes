// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"regexp"

	"github.com/GehirnInc/crypt"
	_ "github.com/GehirnInc/crypt/sha512_crypt" // registers crypt.SHA512
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
)

// statsUsernamePattern keeps the user name a single, harmless HAProxy word.
var statsUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

// validateHAProxyStatsAuth rejects stats credentials that cannot be rendered safely.
func validateHAProxyStatsAuth(cr *marklogicv1.MarklogicCluster) error {
	if cr.Spec.HAProxy == nil || !cr.Spec.HAProxy.Stats.Enabled || !cr.Spec.HAProxy.Stats.Auth.Enabled {
		return nil
	}
	auth := cr.Spec.HAProxy.Stats.Auth
	if !statsUsernamePattern.MatchString(auth.Username) {
		return fmt.Errorf("spec.haproxy.stats.auth.username must be 1-64 characters from A-Z a-z 0-9 . _ @ -")
	}
	if auth.Password == "" {
		return fmt.Errorf("spec.haproxy.stats.auth.password must not be empty when stats authentication is enabled")
	}
	return nil
}

// statsPasswordHash returns a salted SHA-512 crypt hash of the stats password, so the
// rendered configuration (and therefore every ConfigMap, projected volume and log that
// carries it) never contains the password. The salt is derived from the cluster
// and user name, which keeps the output stable: a random salt would change the
// configuration, and trigger a reload, on every reconcile.
func statsPasswordHash(cr *marklogicv1.MarklogicCluster) (string, error) {
	const alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	key := string(cr.UID)
	if key == "" {
		key = cr.Namespace + "/" + cr.Name
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("marklogic-haproxy-stats|" + cr.Spec.HAProxy.Stats.Auth.Username))
	sum := mac.Sum(nil)
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return crypt.SHA512.New().Generate([]byte(cr.Spec.HAProxy.Stats.Auth.Password), append([]byte("$6$"), salt...))
}

// statsUserlist renders the HAProxy user list for the stats page. Invalid credentials
// produce an empty list, which denies everyone, so a mistake can never open the page.
func statsUserlist(cr *marklogicv1.MarklogicCluster) string {
	list := "\nuserlist stats-users\n"
	if validateHAProxyStatsAuth(cr) != nil {
		return list
	}
	hash, err := statsPasswordHash(cr)
	if err != nil {
		return list
	}
	// Single quotes keep HAProxy from expanding the $ characters of the hash.
	return list + fmt.Sprintf("  user %s password '%s'\n", cr.Spec.HAProxy.Stats.Auth.Username, hash)
}
