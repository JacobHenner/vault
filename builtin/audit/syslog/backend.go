// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package syslog

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"sync"

	gsyslog "github.com/hashicorp/go-syslog"
	"github.com/hashicorp/vault/audit"
	"github.com/hashicorp/vault/sdk/helper/salt"
	"github.com/hashicorp/vault/sdk/logical"
)

func Factory(ctx context.Context, conf *audit.BackendConfig, useEventLogger bool) (audit.Backend, error) {
	if conf.SaltConfig == nil {
		return nil, fmt.Errorf("nil salt config")
	}
	if conf.SaltView == nil {
		return nil, fmt.Errorf("nil salt view")
	}

	// Get facility or default to AUTH
	facility, ok := conf.Config["facility"]
	if !ok {
		facility = "AUTH"
	}

	// Get tag or default to 'vault'
	tag, ok := conf.Config["tag"]
	if !ok {
		tag = "vault"
	}

	format, ok := conf.Config["format"]
	if !ok {
		format = "json"
	}
	switch format {
	case "json", "jsonx":
	default:
		return nil, fmt.Errorf("unknown format type %q", format)
	}

	// Check if hashing of accessor is disabled
	hmacAccessor := true
	if hmacAccessorRaw, ok := conf.Config["hmac_accessor"]; ok {
		value, err := strconv.ParseBool(hmacAccessorRaw)
		if err != nil {
			return nil, err
		}
		hmacAccessor = value
	}

	// Check if raw logging is enabled
	logRaw := false
	if raw, ok := conf.Config["log_raw"]; ok {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, err
		}
		logRaw = b
	}

	elideListResponses := false
	if elideListResponsesRaw, ok := conf.Config["elide_list_responses"]; ok {
		value, err := strconv.ParseBool(elideListResponsesRaw)
		if err != nil {
			return nil, err
		}
		elideListResponses = value
	}

	// Get the logger
	logger, err := gsyslog.NewLogger(gsyslog.LOG_INFO, facility, tag)
	if err != nil {
		return nil, err
	}

	cfg, err := audit.NewFormatterConfig(
		audit.WithElision(elideListResponses),
		audit.WithFormat(format),
		audit.WithHMACAccessor(hmacAccessor),
		audit.WithRaw(logRaw),
	)
	if err != nil {
		return nil, err
	}

	b := &Backend{
		logger:       logger,
		saltConfig:   conf.SaltConfig,
		saltView:     conf.SaltView,
		formatConfig: cfg,
	}

	// Configure the formatter for either case.
	f, err := audit.NewEntryFormatter(b.formatConfig, b, audit.WithPrefix(conf.Config["prefix"]))
	if err != nil {
		return nil, fmt.Errorf("error creating formatter: %w", err)
	}

	var w audit.Writer
	switch format {
	case "json":
		w = &audit.JSONWriter{Prefix: conf.Config["prefix"]}
	case "jsonx":
		w = &audit.JSONxWriter{Prefix: conf.Config["prefix"]}
	}

	fw, err := audit.NewEntryFormatterWriter(b.formatConfig, f, w)
	if err != nil {
		return nil, fmt.Errorf("error creating formatter writer: %w", err)
	}

	b.formatter = fw

	return b, nil
}

// Backend is the audit backend for the syslog-based audit store.
type Backend struct {
	logger gsyslog.Syslogger

	formatter    *audit.EntryFormatterWriter
	formatConfig audit.FormatterConfig

	saltMutex  sync.RWMutex
	salt       *salt.Salt
	saltConfig *salt.Config
	saltView   logical.Storage
}

var _ audit.Backend = (*Backend)(nil)

func (b *Backend) GetHash(ctx context.Context, data string) (string, error) {
	salt, err := b.Salt(ctx)
	if err != nil {
		return "", err
	}
	return audit.HashString(salt, data), nil
}

func (b *Backend) LogRequest(ctx context.Context, in *logical.LogInput) error {
	var buf bytes.Buffer
	if err := b.formatter.FormatAndWriteRequest(ctx, &buf, in); err != nil {
		return err
	}

	// Write out to syslog
	_, err := b.logger.Write(buf.Bytes())
	return err
}

func (b *Backend) LogResponse(ctx context.Context, in *logical.LogInput) error {
	var buf bytes.Buffer
	if err := b.formatter.FormatAndWriteResponse(ctx, &buf, in); err != nil {
		return err
	}

	// Write out to syslog
	_, err := b.logger.Write(buf.Bytes())
	return err
}

func (b *Backend) LogTestMessage(ctx context.Context, in *logical.LogInput, config map[string]string) error {
	var buf bytes.Buffer

	temporaryFormatter, err := audit.NewTemporaryFormatter(config["format"], config["prefix"])
	if err != nil {
		return err
	}

	if err = temporaryFormatter.FormatAndWriteRequest(ctx, &buf, in); err != nil {
		return err
	}

	// Send to syslog
	_, err = b.logger.Write(buf.Bytes())
	return err
}

func (b *Backend) Reload(_ context.Context) error {
	return nil
}

func (b *Backend) Salt(ctx context.Context) (*salt.Salt, error) {
	b.saltMutex.RLock()
	if b.salt != nil {
		defer b.saltMutex.RUnlock()
		return b.salt, nil
	}
	b.saltMutex.RUnlock()
	b.saltMutex.Lock()
	defer b.saltMutex.Unlock()
	if b.salt != nil {
		return b.salt, nil
	}
	salt, err := salt.NewSalt(ctx, b.saltView, b.saltConfig)
	if err != nil {
		return nil, err
	}
	b.salt = salt
	return salt, nil
}

func (b *Backend) Invalidate(_ context.Context) {
	b.saltMutex.Lock()
	defer b.saltMutex.Unlock()
	b.salt = nil
}
