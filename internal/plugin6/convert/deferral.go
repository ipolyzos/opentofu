// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package convert

import (
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfplugin6"
)

func DeferralReasonFromProto(reason tfplugin6.Deferred_Reason) providers.DeferralReason {
	switch reason {
	case tfplugin6.Deferred_RESOURCE_CONFIG_UNKNOWN:
		return providers.DeferredBecauseResourceConfigUnknown
	case tfplugin6.Deferred_PROVIDER_CONFIG_UNKNOWN:
		return providers.DeferredBecauseProviderConfigUnknown
	case tfplugin6.Deferred_ABSENT_PREREQ:
		return providers.DeferredBecausePrereqAbsent
	default:
		return providers.DeferredReasonUnknown
	}
}
