#!/bin/sh
# Aileron sandbox Feature: Codex CLI.
#
# Single source of truth for the Codex install recipe. This script runs as
# root during the image build on the Aileron Alpine sandbox base; the
# resulting `codex` binary lands on PATH for the non-root `agent` user.
#
# The npm package and apk prerequisites are mirrored by the in-repo recipe
# table in internal/sandbox/composition/recipes.go; features_test.go asserts
# the two agree, so any drift fails CI.
set -eu

# Prerequisites present in the Alpine base substrate. apk --no-cache keeps the
# step idempotent and image-layer-clean.
apk add --no-cache git nodejs npm ripgrep

# The @openai/codex npm package ships prebuilt musl binaries, so it installs
# cleanly on the Alpine base. npm -g re-runs cleanly, so the install is
# idempotent.
npm install -g @openai/codex
