// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

// Package objectstorage supports cluster-wide object storage credential
// configuration for MarkLogic (AWS S3 and Azure Blob).
package objectstorage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"sort"
)

// Provider identifiers accepted by the MarkLogic Management API credentials endpoint.
const (
	ProviderAWS   = "aws"
	ProviderAzure = "azure"
)

const redacted = "[REDACTED]"

// AWSMaterial is resolved AWS credential material. SessionToken is optional and
// supports STS temporary credentials; when empty it is omitted from the applied
// payload exactly as before (see [SPEC]Object Storage.md, Requirement Review #6).
type AWSMaterial struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
}

func (m AWSMaterial) Fields() map[string]string {
	return map[string]string{
		"accessKey":    m.AccessKey,
		"secretKey":    m.SecretKey,
		"sessionToken": m.SessionToken,
	}
}

// String satisfies fmt.Stringer so material cannot reach logs, events, or errors.
func (m AWSMaterial) String() string { return redacted }

func (m AWSMaterial) Fingerprint(salt string) string {
	return Fingerprint(salt, ProviderAWS, m.Fields())
}

// AzureMaterial is resolved Azure Blob credential material.
type AzureMaterial struct {
	StorageAccount string
	StorageKey     string
}

func (m AzureMaterial) Fields() map[string]string {
	return map[string]string{
		"storageAccount": m.StorageAccount,
		"storageKey":     m.StorageKey,
	}
}

// String satisfies fmt.Stringer so material cannot reach logs, events, or errors.
func (m AzureMaterial) String() string { return redacted }

func (m AzureMaterial) Fingerprint(salt string) string {
	return Fingerprint(salt, ProviderAzure, m.Fields())
}

// Fingerprint returns a salted, non-reversible digest of credential material,
// used to detect rotation without persisting the material or reading it back
// from MarkLogic (which returns non-deterministic ciphertext).
//
// The salt must be stable for the lifetime of the cluster and is derived from
// MarklogicCluster.metadata.uid. Salting matters because status is readable by
// anyone with get on the resource: an unsalted digest would let an observer
// confirm guessed values, and would expose that two clusters share credentials.
func Fingerprint(salt string, provider string, fields map[string]string) string {
	mac := hmac.New(sha256.New, []byte(salt))

	writeLengthPrefixed(mac, provider)

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		writeLengthPrefixed(mac, key)
		writeLengthPrefixed(mac, fields[key])
	}

	return hex.EncodeToString(mac.Sum(nil))
}

// writeLengthPrefixed keeps the digest injective: without the length prefix,
// {"ab": "c"} and {"a": "bc"} would produce the same digest.
func writeLengthPrefixed(w io.Writer, s string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(s)))
	// hash.Hash writes never fail.
	_, _ = w.Write(length[:])
	_, _ = io.WriteString(w, s)
}
