/*
Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

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

package k8sutil

import (
	"fmt"
	"math"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
)

// ErrorClassification categorizes errors for appropriate retry behavior
type ErrorClassification string

const (
	// ErrorClassificationTransient - errors that are temporary and will likely resolve (network glitches, temporary unavailability)
	ErrorClassificationTransient ErrorClassification = "Transient"
	// ErrorClassificationRateLimited - errors due to rate limiting or throttling (EBS cooldown, API quotas)
	ErrorClassificationRateLimited ErrorClassification = "RateLimited"
	// ErrorClassificationResourceQuota - errors due to resource quota exhaustion
	ErrorClassificationResourceQuota ErrorClassification = "ResourceQuota"
	// ErrorClassificationPersistent - errors that require user intervention (invalid config, insufficient permissions)
	ErrorClassificationPersistent ErrorClassification = "Persistent"
	// ErrorClassificationInternal - internal operator errors (logic bugs, unexpected state)
	ErrorClassificationInternal ErrorClassification = "Internal"
)

// RetryConfig holds configuration for retry behavior
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts for transient errors
	MaxRetries int32
	// InitialBackoffSeconds is the initial delay before first retry
	InitialBackoffSeconds int32
	// MaxBackoffSeconds is the maximum delay between retries
	MaxBackoffSeconds int32
	// BackoffMultiplier is the multiplier for exponential backoff (typically 2.0)
	BackoffMultiplier float64
	// JitterFraction adds randomization to backoff (0.0-1.0, typical 0.1)
	JitterFraction float64
	// RateLimitedBackoffMinutes is the delay when rate-limited (e.g., EBS cooldown)
	RateLimitedBackoffMinutes int32
	// QuotaExceededBackoffMinutes is the delay when quota is exceeded
	QuotaExceededBackoffMinutes int32
	// PersistentErrorBackoffMinutes is the delay before alerting user for persistent errors
	PersistentErrorBackoffMinutes int32
}

// DefaultRetryConfig returns sensible defaults for retry behavior
// No hard timeouts - retries continue indefinitely with exponential backoff
// Users can pause via annotation if needed
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:                    5,   // Max retries for transient errors (then reevaluate)
		InitialBackoffSeconds:         5,   // Start with 5 second delay
		MaxBackoffSeconds:             300, // Cap at 5 minutes between retries
		BackoffMultiplier:             2.0, // Double the delay each retry
		JitterFraction:                0.1, // Add ±10% randomness
		RateLimitedBackoffMinutes:     30,  // Fixed delay for rate-limited (e.g., EBS)
		QuotaExceededBackoffMinutes:   10,  // Fixed delay for quota issues
		PersistentErrorBackoffMinutes: 5,   // Periodic check for persistent errors
	}
}

// RetryStrategy determines how to handle a retry situation
type RetryStrategy struct {
	ShouldRetry    bool
	NextRetryTime  time.Time
	Classification ErrorClassification
	Message        string
}

// ClassifyError analyzes an error and determines its classification
func ClassifyError(err error, context string) ErrorClassification {
	if err == nil {
		return ""
	}

	errStr := err.Error()
	lowerErr := strings.ToLower(errStr)

	// Rate limiting patterns
	if strings.Contains(lowerErr, "ratelimit") ||
		strings.Contains(lowerErr, "rate limit") ||
		strings.Contains(lowerErr, "throttl") ||
		strings.Contains(lowerErr, "modification rate") ||
		strings.Contains(lowerErr, "ebscooldown") {
		return ErrorClassificationRateLimited
	}

	// Resource quota patterns
	if strings.Contains(lowerErr, "quota") ||
		strings.Contains(lowerErr, "exceeded") ||
		strings.Contains(lowerErr, "insufficient") {
		return ErrorClassificationResourceQuota
	}

	// Transient patterns
	if strings.Contains(lowerErr, "temporary") ||
		strings.Contains(lowerErr, "timeout") ||
		strings.Contains(lowerErr, "connection") ||
		strings.Contains(lowerErr, "unavailable") ||
		strings.Contains(lowerErr, "temporarily") ||
		strings.Contains(lowerErr, "try again") {
		return ErrorClassificationTransient
	}

	// Persistent patterns (require user intervention)
	if strings.Contains(lowerErr, "permission") ||
		strings.Contains(lowerErr, "forbidden") ||
		strings.Contains(lowerErr, "unauthorized") ||
		strings.Contains(lowerErr, "invalid") ||
		strings.Contains(lowerErr, "not found") ||
		strings.Contains(lowerErr, "not allowed") ||
		strings.Contains(lowerErr, "cannot") {
		return ErrorClassificationPersistent
	}

	// If we can't classify it, assume transient (safer for automatic recovery)
	return ErrorClassificationTransient
}

// CalculateBackoff determines the next retry time based on retry count and error type
// Uses exponential backoff with jitter for transient errors
func CalculateBackoff(config RetryConfig, retryCount int32, classification ErrorClassification) time.Time {
	switch classification {
	case ErrorClassificationRateLimited:
		// Fixed backoff for rate limiting (e.g., EBS cooldown)
		return time.Now().Add(time.Duration(config.RateLimitedBackoffMinutes) * time.Minute)

	case ErrorClassificationResourceQuota:
		// Shorter backoff for quota issues - user might resolve quickly
		return time.Now().Add(time.Duration(config.QuotaExceededBackoffMinutes) * time.Minute)

	case ErrorClassificationPersistent:
		// Even with persistent errors, retry periodically in case user fixes it
		return time.Now().Add(time.Duration(config.PersistentErrorBackoffMinutes) * time.Minute)

	case ErrorClassificationInternal:
		// Internal errors suggest a bug - use standard exponential backoff
		return time.Now().Add(calculateExponentialBackoff(config, retryCount))

	case ErrorClassificationTransient:
		fallthrough
	default:
		// Standard exponential backoff for transient errors
		return time.Now().Add(calculateExponentialBackoff(config, retryCount))
	}
}

// calculateExponentialBackoff computes exponential backoff with jitter
// Formula: min(initial * (multiplier ^ retryCount) * (1 ± jitter), maxBackoff)
func calculateExponentialBackoff(config RetryConfig, retryCount int32) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}

	// Calculate base backoff
	baseBackoff := float64(config.InitialBackoffSeconds) * math.Pow(config.BackoffMultiplier, float64(retryCount))

	// Apply jitter (random ±10% by default)
	jitter := (1.0 - config.JitterFraction + (config.JitterFraction * 2.0 * pseudoRandom()))
	backoff := baseBackoff * jitter

	// Cap at maximum
	maxSeconds := float64(config.MaxBackoffSeconds)
	if backoff > maxSeconds {
		backoff = maxSeconds
	}

	return time.Duration(backoff) * time.Second
}

// Simple pseudo-random number generator for jitter (0.0-1.0)
// Uses current nanoseconds to generate "randomness" without importing math/rand
func pseudoRandom() float64 {
	return float64(time.Now().UnixNano()%1000) / 1000.0
}

// ShouldRetry determines if we should retry based on the error classification and retry count
// Strategy: Retry all errors with exponential backoff and fixed delays, no hard timeouts
// Users can pause via annotation if manual intervention needed
func ShouldRetry(config RetryConfig, retryCount int32, classification ErrorClassification) bool {
	switch classification {
	case ErrorClassificationPersistent:
		// Persistent errors: Retry indefinitely with periodic checks
		// User can pause via annotation or fix the underlying issue
		return true

	case ErrorClassificationRateLimited:
		// Rate-limited errors: Retry indefinitely (external constraint)
		// Will succeed once rate limit window passes
		return true

	case ErrorClassificationResourceQuota:
		// Quota errors: Retry indefinitely
		// Will succeed once user increases quota
		return true

	case ErrorClassificationInternal:
		// Internal errors: Retry with caution
		// Treat like transient but indicate investigation needed
		return retryCount < config.MaxRetries

	case ErrorClassificationTransient:
		fallthrough
	default:
		// Transient errors: Retry with exponential backoff
		// No hard timeout; continue retrying indefinitely if needed
		return retryCount < config.MaxRetries
	}
}

// EvaluateRetry is the main function to determine if and when to retry
func EvaluateRetry(config RetryConfig, status *marklogicv1.VolumeResizeStatus, err error) RetryStrategy {
	if err == nil {
		return RetryStrategy{ShouldRetry: false}
	}

	classification := ClassifyError(err, "volume_resize")

	// Check if we should retry
	retryCount := status.RetryCount
	if !ShouldRetry(config, retryCount, classification) {
		return RetryStrategy{
			ShouldRetry:    false,
			Classification: classification,
			Message:        fmt.Sprintf("Max retries exceeded for %s error", classification),
		}
	}

	// Calculate next retry time
	nextRetryTime := CalculateBackoff(config, retryCount, classification)

	return RetryStrategy{
		ShouldRetry:    true,
		NextRetryTime:  nextRetryTime,
		Classification: classification,
		Message: fmt.Sprintf("Scheduling retry #%d (classification: %s) at %s",
			retryCount+1, classification, nextRetryTime.Format(time.RFC3339)),
	}
}

// UpdateRetryStatus updates the CR status with retry information
func UpdateRetryStatus(status *marklogicv1.VolumeResizeStatus, strategy RetryStrategy, errorMsg string) {
	if !strategy.ShouldRetry {
		// Reset on success
		status.RetryCount = 0
		status.ConsecutiveRetries = 0
		status.LastErrorMessage = ""
		status.ErrorClassification = ""
		return
	}

	// Increment retry counters
	status.RetryCount++
	status.ConsecutiveRetries++

	// Update error information
	status.LastErrorMessage = errorMsg
	status.ErrorClassification = string(strategy.Classification)

	// Update next retry time
	nextRetryTime := metav1.NewTime(strategy.NextRetryTime)
	status.NextRetryTime = &nextRetryTime

	// Record when this retry was attempted
	now := metav1.Now()
	status.LastRetryTime = &now
}

// CheckIfPausedByAnnotation checks if operator should pause retries pending user intervention
// Annotation format: marklogic.com/pause-resize=true
func CheckIfPausedByAnnotation(cr interface {
	GetAnnotations() map[string]string
}) bool {
	annotations := cr.GetAnnotations()
	if annotations == nil {
		return false
	}

	if pauseVal, exists := annotations["marklogic.com/pause-resize"]; exists {
		return strings.ToLower(pauseVal) == "true"
	}

	return false
}

// CheckIfRetryBlockedByPersistentError checks if retries are blocked due to persistent error
// Returns true if there's a persistent error that requires user intervention before retrying
func CheckIfRetryBlockedByPersistentError(status *marklogicv1.VolumeResizeStatus) bool {
	if status == nil {
		return false
	}

	classification := status.ErrorClassification
	if classification == string(ErrorClassificationPersistent) {
		// Check if we're already past the alert threshold
		if status.LastRetryTime != nil {
			timeSinceLast := time.Since(status.LastRetryTime.Time)
			alertThreshold := 5 * time.Minute
			if timeSinceLast > alertThreshold {
				return true
			}
		}
	}

	return false
}
