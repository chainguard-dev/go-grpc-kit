#!/usr/bin/env bash

# Copyright 2022 Chainguard, Inc.
# SPDX-License-Identifier: Apache-2.0

set -o errexit
set -o nounset
set -o pipefail

echo === Update Deps for Golang
go mod tidy -compat=1.18
