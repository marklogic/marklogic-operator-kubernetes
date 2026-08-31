// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package mlmanage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const credentialsPropertiesPath = "/manage/v2/credentials/properties"

// AWSCredentials is the AWS credential material applied cluster-wide.
// MarkLogic also accepts an optional session-token; it is intentionally not
// supported because it expires, cannot be refreshed by the operator, and is
// returned in plaintext by the Management API.
type AWSCredentials struct {
	AccessKey string
	SecretKey string
}

// AzureCredentials is the Azure Blob credential material applied cluster-wide.
type AzureCredentials struct {
	StorageAccount string
	StorageKey     string
}

// CredentialsError reports a failed credentials operation using only the HTTP
// status and MarkLogic's own error fields. The request body is never included
// because it carries credential material.
type CredentialsError struct {
	// StatusCode is 0 when the request never produced a response.
	StatusCode  int
	MessageCode string
	Message     string
	Err         error
}

func (e *CredentialsError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("management api credentials request failed: %v", e.Err)
	}
	switch {
	case e.MessageCode != "" && e.Message != "":
		return fmt.Sprintf("management api credentials request returned status %d (%s): %s", e.StatusCode, e.MessageCode, e.Message)
	case e.Message != "":
		return fmt.Sprintf("management api credentials request returned status %d: %s", e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("management api credentials request returned status %d", e.StatusCode)
	}
}

func (e *CredentialsError) Unwrap() error { return e.Err }

func (c *managementClient) EnsureAWSCredentials(ctx context.Context, config AWSCredentials) error {
	payload, err := BuildAWSCredentialsPayload(config)
	if err != nil {
		return err
	}
	return c.putCredentials(ctx, payload)
}

func (c *managementClient) EnsureAzureCredentials(ctx context.Context, config AzureCredentials) error {
	payload, err := BuildAzureCredentialsPayload(config)
	if err != nil {
		return err
	}
	return c.putCredentials(ctx, payload)
}

// BuildAWSCredentialsPayload returns the Management API representation for AWS credentials.
func BuildAWSCredentialsPayload(config AWSCredentials) (map[string]any, error) {
	if err := validateRequiredCredentialFields(map[string]string{
		"AWS access key": config.AccessKey,
		"AWS secret key": config.SecretKey,
	}); err != nil {
		return nil, err
	}

	// MarkLogic selects the provider from this body field. The documented
	// ?type= query parameter has no effect on how the payload is parsed.
	return map[string]any{
		"type":       "aws",
		"access-key": config.AccessKey,
		"secret-key": config.SecretKey,
	}, nil
}

// BuildAzureCredentialsPayload returns the Management API representation for Azure credentials.
func BuildAzureCredentialsPayload(config AzureCredentials) (map[string]any, error) {
	if err := validateRequiredCredentialFields(map[string]string{
		"Azure storage account": config.StorageAccount,
		"Azure storage key":     config.StorageKey,
	}); err != nil {
		return nil, err
	}

	// Azure fails with 400 MANAGE-INVALIDPAYLOAD if this field is absent.
	return map[string]any{
		"type":            "azure",
		"storage-account": config.StorageAccount,
		"storage-key":     config.StorageKey,
	}, nil
}

// validateRequiredCredentialFields reports missing fields by name only, never by value.
func validateRequiredCredentialFields(fields map[string]string) error {
	for field, value := range fields {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", field)
		}
	}
	return nil
}

func (c *managementClient) putCredentials(ctx context.Context, payload map[string]any) error {
	data, statusCode, err := c.doJSON(ctx, http.MethodPut, credentialsPropertiesPath, nil, payload, http.StatusNoContent)
	if statusCode == http.StatusNoContent {
		return err
	}

	// doJSON's own error embeds the response body, so it is discarded here in
	// favour of a sanitised error built from the status and MarkLogic's fields.
	if statusCode == 0 {
		return &CredentialsError{Err: err}
	}

	credentialsErr := &CredentialsError{StatusCode: statusCode}
	credentialsErr.MessageCode, credentialsErr.Message = parseManagementErrorResponse(data)
	return credentialsErr
}

// parseManagementErrorResponse extracts MarkLogic's structured error fields.
// The body is treated as untrusted: only these two fields are ever surfaced.
func parseManagementErrorResponse(body []byte) (messageCode string, message string) {
	var envelope struct {
		ErrorResponse struct {
			MessageCode string `json:"messageCode"`
			Message     string `json:"message"`
		} `json:"errorResponse"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", ""
	}
	return envelope.ErrorResponse.MessageCode, envelope.ErrorResponse.Message
}
