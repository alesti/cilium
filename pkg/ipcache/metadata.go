// SPDX-License-Identifier: Apache-2.0
// Copyright 2018-2019 Authors of Cilium

package ipcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/cilium/cilium/pkg/controller"
	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/identity/cache"
	"github.com/cilium/cilium/pkg/labels"
	cidrlabels "github.com/cilium/cilium/pkg/labels/cidr"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/source"

	"github.com/sirupsen/logrus"
)

var (
	// idMDMU protects the identityMetadata map.
	//
	// If this mutex will be held at the same time as the IPCache mutex,
	// this mutex must be taken first and then take the IPCache mutex in
	// order to prevent deadlocks.
	idMDMU lock.RWMutex
	// identityMetadata maps IP prefixes (x.x.x.x/32) to their labels.
	//
	// When allocating an identity to associate with each prefix, the
	// identity allocation routines will merge this set of labels into the
	// complete set of labels used for that local (CIDR) identity,
	// thereby associating these labels with each prefix that is 'covered'
	// by this prefix. Subsequently these labels may be matched by network
	// policy and propagated in monitor output.
	identityMetadata = make(map[string]labels.Labels)

	// ErrLocalIdentityAllocatorUninitialized is an error that's returned when
	// the local identity allocator is uninitialized.
	ErrLocalIdentityAllocatorUninitialized = errors.New("local identity allocator uninitialized")
)

// UpsertMetadata upserts a given IP and its corresponding labels associated
// with it into the identityMetadata map. The given labels are not modified nor
// is its reference saved, as they're copied when inserting into the map.
func UpsertMetadata(prefix string, lbls labels.Labels) {
	l := labels.NewLabelsFromModel(nil)
	l.MergeLabels(lbls)

	idMDMU.Lock()
	if cur, ok := identityMetadata[prefix]; ok {
		l.MergeLabels(cur)
	}
	identityMetadata[prefix] = l
	idMDMU.Unlock()
}

// GetIDMetadataByIP returns the associated labels with an IP. The caller must
// not modifying the returned object as it's a live reference to the underlying
// map.
func GetIDMetadataByIP(prefix string) labels.Labels {
	idMDMU.RLock()
	defer idMDMU.RUnlock()
	return identityMetadata[prefix]
}

// InjectLabels injects labels from the identityMetadata (IDMD) map into the
// identities used for the prefixes in the IPCache. The given source is the
// source of the caller, as inserting into the IPCache requires knowing where
// this updated information comes from.
//
// Note that as this function iterates through the IDMD, if it detects a change
// in labels for a given prefix, then this might allocate a new identity. If a
// prefix was previously associated with an identity, it will get deallocated,
// so a balance is kept.
func InjectLabels(src source.Source, updater identityUpdater, triggerer policyTriggerer) error {
	if IdentityAllocator == nil || !IdentityAllocator.IsLocalIdentityAllocatorInitialized() {
		return ErrLocalIdentityAllocatorUninitialized
	}

	if IPIdentityCache.k8sSyncedChecker != nil &&
		!IPIdentityCache.k8sSyncedChecker.K8sCacheIsSynced() {
		return errors.New("k8s cache not fully synced")
	}

	var (
		// trigger is true when we need to trigger policy recalculations.
		trigger bool
		// toUpsert stores IPKeyPairs to upsert into the ipcache.
		toUpsert = make(map[string]Identity)
		// idsToPropagate stores the identities that must be updated via the
		// selector cache.
		idsToPropagate = make(map[identity.NumericIdentity]labels.LabelArray)
	)

	idMDMU.Lock()
	defer idMDMU.Unlock()

	for prefix, lbls := range identityMetadata {
		id, isNew, err := injectLabels(prefix, lbls)
		if err != nil {
			return fmt.Errorf("failed to allocate new identity for IP %v: %w", prefix, err)
		}

		hasKubeAPIServerLabel := lbls.Has(
			labels.LabelKubeAPIServer[labels.IDNameKubeAPIServer],
		)
		// Identities with the kube-apiserver label should always be upserted
		// if there was a change in their labels. This is especially important
		// for IDs such as kube-apiserver or host (which can have the
		// kube-apiserver label when the kube-apiserver is deployed within the
		// cluster), or CIDR IDs for kube-apiservers deployed outside of the
		// cluster.
		// Also, any new identity should be upserted, regardless.
		if hasKubeAPIServerLabel || isNew {
			tmpSrc := src
			if hasKubeAPIServerLabel {
				// Overwrite the source because any IP associated with the
				// kube-apiserver takes the strongest precedence. This is
				// because we need to overwrite Local if only the local node IP
				// has been upserted into the ipcache first.
				//
				// Also, trigger policy recalculations to update kube-apiserver
				// identity.
				newLbls := id.Labels
				tmpSrc = source.KubeAPIServer
				trigger = true
				// If any reserved ID has changed, update its labels.
				if id.IsReserved() {
					identity.AddReservedIdentityWithLabels(id.ID, newLbls)
				}
				idsToPropagate[id.ID] = newLbls.LabelArray()
			}

			toUpsert[prefix] = Identity{
				ID:     id.ID,
				Source: tmpSrc,
			}
		} else {
			// Unlikely, but to balance the allocation / release
			// we must either add the identity to `toUpsert`, or
			// immediately release it again. Otherwise it will leak.
			if _, err := IdentityAllocator.Release(context.TODO(), id, false); err != nil {
				log.WithError(err).WithFields(logrus.Fields{
					logfields.IPAddr: prefix,
					logfields.Labels: lbls,
				}).Error(
					"Failed to release assigned identity during label injection, this might be a leak.",
				)
			} else {
				log.WithFields(logrus.Fields{
					logfields.IPAddr:   prefix,
					logfields.Identity: id,
				}).Debug(
					"Released identity to balance reference counts",
				)
			}
		}
	}

	// Recalculate policy first before upserting into the ipcache.
	if trigger {
		// GH-17962: Refactor to call (*Daemon).UpdateIdentities(), instead of
		// re-implementing the same logic here. It will also allow removing the
		// dependencies that are passed into this function.

		// Accumulate the desired policy map changes as the identities have
		// been updated with new labels.
		var wg sync.WaitGroup
		updater.UpdateIdentities(idsToPropagate, nil, &wg)
		wg.Wait()

		// This will take the accumulated policy map changes from the above,
		// and realizes it into the datapath.
		triggerer.TriggerPolicyUpdates(false, "kube-apiserver identity updated")
	}

	IPIdentityCache.mutex.Lock()
	defer IPIdentityCache.mutex.Unlock()
	for ip, id := range toUpsert {
		hIP, key := IPIdentityCache.getHostIPCache(ip)
		meta := IPIdentityCache.getK8sMetadata(ip)
		if _, err := IPIdentityCache.upsertLocked(ip, hIP, key, meta, Identity{
			ID:     id.ID,
			Source: id.Source,
		}); err != nil {
			return fmt.Errorf("failed to upsert %s into ipcache with identity %d: %w", ip, id.ID, err)
		}
	}

	return nil
}

// injectLabels will allocate an identity for the given prefix and the given
// labels. The caller of this function can expect that an identity is newly
// allocated with reference count of 1 or an identity is looked up and its
// reference count is incremented.
//
// The release of the identity must be managed by the caller, except for the
// case where a CIDR policy exists first and then the kube-apiserver policy is
// applied. This is because the CIDR identities before the kube-apiserver
// policy is applied will need to be converted (released and re-allocated) to
// account for the new kube-apiserver label that will be attached to them. This
// is a known issue, see GH-17962 below.
func injectLabels(prefix string, lbls labels.Labels) (*identity.Identity, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), option.Config.IPAllocationTimeout)
	defer cancel()

	// If no other labels are associated with this IP, we assume that it's
	// outside of the cluster and hence needs a CIDR identity.
	if lbls.Equals(labels.LabelKubeAPIServer) {
		// GH-17962: Handle the following case:
		//   1) Apply ToCIDR policy (matching IPs of kube-apiserver)
		//   2) Apply kube-apiserver policy
		//
		// Possible implementation:
		//   Lookup CIDR ID => get all CIDR labels minus kube-apiserver label.
		//   If found, means that ToCIDR policy already applied. Convert CIDR
		//   IDs to include a new identity with kube-apiserver label. We don't
		//   need to remove old entries from ipcache because the caller will
		//   overwrite the ipcache entry anyway.

		return injectLabelsForCIDR(prefix, lbls)
	}

	return IdentityAllocator.AllocateIdentity(ctx, lbls, false)
}

// injectLabelsForCIDR will allocate a CIDR identity for the given prefix. The
// release of the identity must be managed by the caller.
func injectLabelsForCIDR(p string, lbls labels.Labels) (*identity.Identity, bool, error) {
	var prefix string

	ip := net.ParseIP(p)
	if ip == nil {
		return nil, false, fmt.Errorf("Invalid IP inserted into IdentityMetadata: %s", prefix)
	} else if ip.To4() != nil {
		prefix = p + "/32"
	} else {
		prefix = p + "/128"
	}

	_, cidr, err := net.ParseCIDR(prefix)
	if err != nil {
		return nil, false, err
	}

	allLbls := cidrlabels.GetCIDRLabels(cidr)
	allLbls.MergeLabels(lbls)

	log.WithFields(logrus.Fields{
		logfields.CIDR:   cidr,
		logfields.Labels: lbls, // omitting allLbls as CIDR labels would make this massive
	}).Debug(
		"Injecting CIDR labels for prefix",
	)

	return allocate(cidr, allLbls)
}

// FilterMetadataByLabels returns all the prefixes inside the identityMetadata
// map which contain the given labels. Note that `filter` is a subset match,
// not a full match.
func FilterMetadataByLabels(filter labels.Labels) []string {
	idMDMU.RLock()
	defer idMDMU.RUnlock()

	var matching []string
	sortedFilter := filter.SortedList()
	for prefix, lbls := range identityMetadata {
		if bytes.Contains(lbls.SortedList(), sortedFilter) {
			matching = append(matching, prefix)
		}
	}
	return matching
}

// RemoveLabelsFromIPs wraps RemoveLabels to provide a convenient method for
// the caller to remove all given prefixes at once. This function will trigger
// policy update and recalculation if necessary on behalf of the caller if any
// changes to the kube-apiserver were detected.
//
// Identities allocated by InjectLabels() may be released by RemoveLabels().
//
// A prefix will only be removed from the IDMD if the set of labels becomes
// empty.
func RemoveLabelsFromIPs(
	m map[string]labels.Labels,
	src source.Source,
	updater identityUpdater,
	triggerer policyTriggerer,
) {
	var (
		idsToAdd    = make(map[identity.NumericIdentity]labels.LabelArray)
		idsToDelete = make(map[identity.NumericIdentity]labels.LabelArray)
	)
	for prefix, lbls := range m {
		id, exists := IPIdentityCache.LookupByIP(prefix)
		if !exists {
			continue
		}
		// Insert to propagate the updated set of labels after removal.
		var la labels.LabelArray
		if l := RemoveLabels(prefix, lbls, src); l != nil {
			la = l.LabelArray()
		}
		idsToDelete[id.ID] = la
		if len(la) > 0 {
			// If for example kube-apiserver label is removed from
			// a remote-node, then RemoveLabels() will return a
			// non-empty set representing the new full set of
			// labels to associate with the node. In order to
			// propagate the new identity, we must emit a delete
			// event for the old identity and then an add event for
			// the new identity.
			idsToAdd[id.ID] = la
		}
	}
	if len(idsToDelete) > 0 {
		var wg sync.WaitGroup
		// SelectorCache.UpdateIdentities() asks for callers to avoid
		// handing the same identity in both 'adds' and 'deletes'
		// parameters here, so make two calls. These changes will not
		// be propagated to the datapath until later.
		updater.UpdateIdentities(nil, idsToDelete, &wg)
		updater.UpdateIdentities(idsToAdd, nil, &wg)
		wg.Wait()

		triggerer.TriggerPolicyUpdates(false, "kube-apiserver identity updated by removal")
	}
}

// RemoveLabels removes the given labels association with the given prefix. The
// leftover labels are returned, if any.
//
// Identities are deallocated and their subequent entry in the IPCache is
// removed if the prefix is no longer associated with any labels.
//
// It is the responsibility of the caller to trigger policy recalculation after
// calling this function.
func RemoveLabels(prefix string, lbls labels.Labels, src source.Source) labels.Labels {
	idMDMU.Lock()
	defer idMDMU.Unlock()

	l, ok := identityMetadata[prefix]
	if !ok {
		return nil
	}

	l = l.Remove(lbls)
	if len(l) != 0 { // Labels left over, do not deallocate
		identityMetadata[prefix] = l
		return l
	}

	// No labels left, perform deallocation

	IPIdentityCache.Lock()
	defer IPIdentityCache.Unlock()
	delete(identityMetadata, prefix)
	id, exists := IPIdentityCache.LookupByIPRLocked(prefix)
	if !exists {
		return nil
	}
	realID := IdentityAllocator.LookupIdentityByID(context.TODO(), id.ID)
	if realID == nil {
		return nil
	}
	released, err := IdentityAllocator.Release(context.TODO(), realID, false)
	if err != nil {
		log.WithError(err).WithFields(logrus.Fields{
			logfields.IPAddr:         prefix,
			logfields.Labels:         lbls,
			logfields.Identity:       realID,
			logfields.IdentityLabels: realID.Labels,
		}).Error(
			"Failed to release assigned identity to IP while removing label association, this might be a leak.",
		)
	}
	if released {
		if lbls.Has(labels.LabelKubeAPIServer[labels.IDNameKubeAPIServer]) {
			src = source.KubeAPIServer
		}
		IPIdentityCache.deleteLocked(prefix, src)
	}
	return nil
}

// TriggerLabelInjection triggers the label injection controller to iterate
// through the IDMD and potentially allocate new identities based on any label
// changes.
//
// The following diagram describes the relationship between the label injector
// triggered here and the callers/callees.
//
//      +------------+  (1)        (1)  +-----------------------------+
//      | EP Watcher +-----+      +-----+ CN Watcher / Node Discovery |
//      +-----+------+   W |      | W   +------+----------------------+
//            |            |      |            |
//            |            v      v            |
//            |            +------+            |
//            |            | IDMD |            |
//            |            +------+            |
//            |               ^                |
//            |               |                |
//            |           (3) |R               |
//            | (2)    +------+--------+   (2) |
//            +------->|Label Injector |<------+
//           Trigger   +-------+-------+ Trigger
//                         (4) |W
//                             |
//                             v
//                           +---+
//                           |IPC|
//                           +---+
//      legend:
//      * W means write
//      * R means read
func (ipc *IPCache) TriggerLabelInjection(src source.Source, sc identityUpdater, pt policyTriggerer) {
	// GH-17829: Would also be nice to have an end-to-end test to validate
	//           on upgrade that there are no connectivity drops when this
	//           channel is preventing transient BPF entries.

	// This controller is for retrying this operation in case it fails. It
	// should eventually succeed.
	ipc.UpdateController(
		"ipcache-inject-labels",
		controller.ControllerParams{
			DoFunc: func(context.Context) error {
				if err := InjectLabels(src, sc, pt); err != nil {
					return fmt.Errorf("failed to inject labels into ipcache: %w", err)
				}
				return nil
			},
		},
	)
}

type identityUpdater interface {
	UpdateIdentities(added, deleted cache.IdentityCache, wg *sync.WaitGroup)
}

type policyTriggerer interface {
	TriggerPolicyUpdates(bool, string)
}