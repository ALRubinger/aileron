#!/bin/sh
# Aileron sandbox Feature: Claude Code CLI.
#
# Single source of truth for the Claude install recipe. This script runs as
# root during the image build on the Aileron Alpine sandbox base; the
# resulting `claude` binary lands on PATH for the non-root `agent` user.
#
# The npm package and apk prerequisites are mirrored by the in-repo recipe
# table in internal/sandbox/composition/recipes.go; features_test.go asserts
# the two agree, so any drift fails CI.
set -eu

# Prerequisites present in the Alpine base substrate. apk --no-cache keeps the
# step idempotent and image-layer-clean.
apk add --no-cache git nodejs npm ripgrep

# The @anthropic-ai/claude-code npm package installs cleanly on musl/Alpine.
# npm -g re-runs cleanly, so the install is idempotent.
npm install -g @anthropic-ai/claude-code
