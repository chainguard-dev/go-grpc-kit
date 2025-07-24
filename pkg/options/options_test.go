/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package options

import (
	"os"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestGetEnv(t *testing.T) {
	old := os.Getenv("GRPC_CLIENT_MAX_RETRY")
	defer os.Setenv("GRPC_CLIENT_MAX_RETRY", old)

	want := envStruct{
		EnableClientHandlingTimeHistogram:      true,
		EnableClientStreamReceiveTimeHistogram: true,
		EnableClientStreamSendTimeHistogram:    true,
		GrpcClientMaxRetry:                     42,
	}
	t.Run("can change env right before usage", func(t *testing.T) {
		os.Setenv("GRPC_CLIENT_MAX_RETRY", strconv.Itoa(int(want.GrpcClientMaxRetry)))
		if diff := cmp.Diff(want, state().env); diff != "" {
			t.Errorf("getEnv() -want,+got: %s", diff)
		}
	})
	t.Run("but cannot change after usage", func(t *testing.T) {
		os.Setenv("GRPC_CLIENT_MAX_RETRY", strconv.Itoa(int(want.GrpcClientMaxRetry+10)))
		if diff := cmp.Diff(want, state().env); diff != "" {
			t.Errorf("getEnv() -want,+got: %s", diff)
		}
	})
}
