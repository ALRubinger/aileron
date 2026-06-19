#!/bin/sh
# Aileron sandbox Feature: GitHub CLI (gh).
#
# Single source of truth for the gh install recipe when gh ships as a
# composable Feature. This script runs as root during the image build on the
# Aileron Alpine sandbox base; the resulting `gh` binary lands on PATH for the
# non-root `agent` user.
#
# Unlike the agent Features (claude, codex), gh is an apk-only install with no
# npm package: it is a CLI-capability Feature, not an npm agent recipe. Its
# parity target is the base images/sandbox-base/Containerfile `github-cli`
# line, not the npm-keyed recipe table in
# internal/sandbox/composition/recipes.go.
set -eu

# github-cli ships only in Alpine's community repo, which alpine:3.24 does not
# enable by default. Scope the community repo to this single apk invocation via
# --repository (pinned to v3.24 to match the base FROM tag) so the install
# reproduces the base Containerfile's community-repo scoping exactly. apk
# --no-cache keeps the step idempotent and image-layer-clean.
apk add --no-cache \
    --repository=https://dl-cdn.alpinelinux.org/alpine/v3.24/community \
    github-cli
