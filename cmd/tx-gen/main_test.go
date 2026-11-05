package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunPrintsVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-version"}, &stdout, &stderr)
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), "tx-gen ")
	assert.Contains(t, stdout.String(), "commit none")
	assert.Empty(t, stderr.String())
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	assert.Equal(t, 0, run([]string{"-h"}, &stdout, &stderr))
	assert.Contains(t, stderr.String(), "-version")
}

func TestRunRejectsUnknownFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	assert.Equal(t, 2, run([]string{"-nope"}, &stdout, &stderr))
	assert.Contains(t, stderr.String(), "flag provided but not defined")
}

func TestRunReportsConfigErrors(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("INSTANCE_ID", "")
	var stdout, stderr bytes.Buffer
	assert.Equal(t, 1, run(nil, &stdout, &stderr))
	assert.Contains(t, stderr.String(), "config: ADMIN_TOKEN is required")
}

func TestBuildInfoUsesLdflags(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })

	version = "v9.9.9"
	assert.Equal(t, "v9.9.9", buildInfo().Version)

	version = "dev"
	assert.NotEmpty(t, buildInfo().Version)
}
