package updater

import (
	"crypto/sha256"
	"encoding/binary"
)

// rolloutEligible decides whether a frame is inside a release's staged
// rollout (spec §14 Release channels: "Support staged rollouts").
//
// Two mechanisms, ORed:
//  1. allow-list: the frame's device id is in rel.RolloutDevices
//     (canary / development frames are named explicitly);
//  2. percentage: a stable per-device bucket in [0,100) derived from
//     SHA-256(device id). Bucket < rel.RolloutPercent means "in".
//
// The bucket is stable across restarts and servers, so a frame's
// rollout membership never flickers; raising the percentage only ever
// adds frames (a 10% cohort is a subset of the 25% cohort), which is
// what makes gradual expansion and pause-safe rollback predictable.
// rolloutEligibleWith decides membership from the EFFECTIVE rollout
// values — the server's serving override when present, otherwise the
// signed fields (see ServingOverride).
func rolloutEligibleWith(rel Release, percent int, devices []string, deviceID string) bool {
	for _, d := range devices {
		if d == deviceID {
			return true
		}
	}
	if percent >= 100 {
		return true
	}
	if percent <= 0 {
		return false
	}
	if deviceID == "" {
		// No identity: refuse rather than guess (a frame without an
		// identity cannot be paired or update safely anyway).
		return false
	}
	sum := sha256.Sum256([]byte("openframe-rollout/" + rel.Channel + "/" + rel.Version + "/" + deviceID))
	n := binary.BigEndian.Uint64(sum[:8])
	bucket := int(n % 100)
	return bucket < percent
}

func rolloutEligible(rel Release, deviceID string) bool {
	return rolloutEligibleWith(rel, rel.RolloutPercent, rel.RolloutDevices, deviceID)
}

// RolloutEligible is the exported decision function: eligible under
// the effective rollout values (serving override when the server
// provides one, otherwise the signed fields).
func RolloutEligible(rel Release, percent int, devices []string, deviceID string) bool {
	return rolloutEligibleWith(rel, percent, devices, deviceID)
}
