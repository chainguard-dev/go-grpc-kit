/*
Copyright 2023 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package retry

import (
	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
)

type Predicate struct {
	Match   func(method string) bool
	Options []grpc_retry.CallOption
}

type Policy []Predicate
