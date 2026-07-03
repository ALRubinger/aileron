#!/bin/sh
# Aileron sandbox Feature: AWS CLI (aws-cli).
#
# Single source of truth for the aws-cli install recipe when aws-cli ships as a
# composable Feature. This script runs as root during the image build on the
# Aileron Alpine sandbox base; the resulting `aws` binary lands on PATH for the
# non-root `agent` user.
#
# Unlike the agent Features (claude, codex), aws-cli is an apk-only install with
# no npm package: it is a CLI-capability Feature, not an npm agent recipe. Its
# parity target is Alpine's community `aws-cli` package (the AWS CLI v2 build),
# not the npm-keyed recipe table in internal/sandbox/composition/recipes.go.
#
# The credential convention (which scheme seals aws-cli's outbound requests and
# which placeholder pair the launcher plants) lives entirely in this Feature's
# devcontainer-feature.json under customizations.aileron.credential. This script
# never reads or bakes AWS credentials.
set -eu

# aws-cli ships only in Alpine's community repo, which alpine:3.24 does not
# enable by default. Scope the community repo to this single apk invocation via
# --repository (pinned to v3.24 to match the base FROM tag) so the install
# reproduces the base Containerfile's community-repo scoping exactly. apk
# --no-cache keeps the step idempotent and image-layer-clean.
apk add --no-cache \
    --repository=https://dl-cdn.alpinelinux.org/alpine/v3.24/community \
    aws-cli
