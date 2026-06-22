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

# Apache-2.0 redistribution obligation. The @openai/codex npm package declares
# "license": "Apache-2.0" but ships neither the license text nor the upstream
# NOTICE, so redistributing it in this image without them would breach the one
# requirement Apache-2.0 actually imposes (4(d): include the License and
# reproduce NOTICE attributions). These two files are vendored alongside this
# script from openai/codex's LICENSE and NOTICE; the published-image build COPYs
# the whole Feature tree to /tmp/aileron-features before running install.sh, so
# they are present here at build time. We persist them to the conventional
# /usr/share/licenses/<pkg> path (Alpine's third-party license location) so they
# survive the build's removal of the Feature tree and are discoverable in the
# running image. The directory name "@openai/codex" mirrors the npm package id.
license_dir="/usr/share/licenses/@openai/codex"
mkdir -p "$license_dir"
cp "$(dirname "$0")/LICENSE" "$license_dir/LICENSE"
cp "$(dirname "$0")/NOTICE" "$license_dir/NOTICE"
