# Device-class-aware OSD PodDisruptionBudgets

## Goal

When a CephCluster uses multiple OSD device classes (for example `hdd` and `ssd`) with pools strictly isolated by `spec.deviceClass`, allow concurrent drains across classes. Draining an HDD node in zone-a must not block draining an SSD node in zone-b.

Tracking issue: [rook/rook#16976](https://github.com/rook/rook/issues/16976).

## Status and plan tracking

This file is the working design and the implementation tracker. The user-facing
design doc [ceph-managed-disruptionbudgets.md](design/ceph/ceph-managed-disruptionbudgets.md)
is updated only at the end, after a working and tested version and a local review.

Locked decisions:

- **No CRD opt-in flag.** The unclassified-pool fallback guarantees no regression,
  so the device-class behavior activates automatically when every pool has a
  non-empty `deviceClass`. No new field on the CephCluster CRD.
- **Per-class PG cleanliness is implemented now**, not deferred. It is the one
  genuinely new external Ceph behavior and is required for Case 5.

Implementation progress (all changes live in
`pkg/operator/ceph/disruption/clusterdisruption/` plus one helper in
`pkg/daemon/ceph/client/deviceclass_pg.go`; no CRD changes):

- [x] `deviceclass_pg.go`: CRUSH-based device-class mapping (`GetDeviceClassPools` returns
  per-class pools, per-class failure-domain types, and `HasSpanningPool`) and per-class PG
  cleanliness (`ArePoolsClean`). Resolved in two bounded queries (`osd crush dump` +
  `osd pool ls detail`); see [the classIsolated decision](#built-in-pools-and-the-classisolated-decision).
- [x] `pools.go`: `minimumFailureDomainFromTypes` (per-class minimum failure domain from CRUSH
  failure-domain types); `processPools` keeps its original CR-based role for the fallback path.
- [x] `reconcile.go`: derive `classIsolated` from CRUSH (`HasSpanningPool`), branch to the
  class-aware path, and run the fallback handoff cleanup otherwise; legacy-key read for
  mid-drain upgrade.
- [x] `osd_deviceclass.go`: per-class failure-domain enumeration
  (`getOSDFailureDomainsByClass`).
- [x] `osd_deviceclass.go`: per-class ConfigMap state (`setPDBConfigForClass`/
  `resetPDBConfigForClass`) namespaced under the `device-class/<class>/<base>` prefix.
- [x] `osd.go`/`osd_deviceclass.go`: device-class `NotIn`/`In` selector clauses on the
  default and blocking PDBs; default PDB no longer deleted in class-isolated mode.
- [x] `osd_deviceclass.go`: three-phase write-ordering apply (`applyDeviceClassPDBs`).
- [x] `osd_deviceclass.go`: `updateNoout` union fix (`updateNooutForClasses`).

Review fixes folded in:

- [x] CRUSH-based gate closes the `.nfs`/directly-created-pool gap (PO1) and the CR-versus-CRUSH
  drift, and makes the gate and cleanliness check share one source of truth so they cannot
  diverge (P1).
- [x] Two bounded Ceph queries replace the per-pool N+1, and per-class PG cleanliness is queried
  only for classes with a down OSD or active drain, not on idle reconciles (P2).
- [x] Fallback handoff (`cleanupDeviceClassArtifacts`): when isolation is lost, delete
  class-scoped blocking PDBs and clear per-class keys before running the fallback (R1).
- [x] Per-class keys namespaced under `device-class/` so they cannot collide with legacy keys
  and are cleanable by prefix (D4).
- [x] Phase 3 skips the redundant write when phase 1 already wrote the desired set (D1).
- [x] Idle fast path (`reconcileIdleOSDPDBs`): when no OSD is down and no drain bookkeeping
  exists, the default PDB is applied with no Ceph reads, so idle clusters and clusters that
  never classify their pools stay off the CRUSH/PG queries and a transient Ceph read failure
  cannot block PDB management (N1).
- [x] Per-`(fd, class)` noout timestamps (`nooutTimestampKey`) so a second class draining a
  failure domain already drained by another gets its own maintenance window (D3/N4).
- [ ] **Deferred (noted):** consolidate the CRUSH-rule parser with `extractPoolDetails`
  (P4, comment added cross-referencing it).
- [x] Unit tests: Cases 1 through 5 plus the no-double-match invariant
  (`TestApplyDeviceClassPDBs`), the Case 3 to Case 5 transition
  (`TestApplyDeviceClassPDBsTransition`), per-class cleanliness end to end
  (`TestReconcilePDBsForOSDsByClass`), `getFailureDomainsByDeviceClass`,
  `getOSDFailureDomainsByClass`, and the client helpers. Existing fallback-path tests
  still pass unchanged.
- [x] Build, unit tests, linter (`go build`, `go vet`, `go test`, `golangci-lint` all clean
  on the touched packages).
- [x] Manual flow on the local Lima cluster. Validated live across two iterations:
  - First (CR-based build): fallback path unchanged; the class-aware path activates once
    every pool is classified; per-class reconcile runs against real Ceph; the default PDB is
    retained (not deleted) when an OSD is down, with the `osd NotIn` exclusion and no class
    clause. Confirmed `pg ls-by-pool` returns the `{"pg_ready":...,"pg_stats":[...]}` wrapper.
  - After the CRUSH-based refactor: rebuilt and redeployed; the gate
    (`osd crush dump` + `osd pool ls detail`) runs against real Ceph with no parse errors and
    correctly determines the default cluster is not isolated (`.mgr` spans classes) -> fallback.
    Success path of the int-keyed join confirmed on real output: a freshly created `hdd` pool
    shows `crush_rule=1` in `osd pool ls detail` and rule 1 = `take default~hdd` / `fd host` in
    `osd crush dump`, which the join resolves to class `hdd` / failure domain `host` (only `.mgr`
    keeps the cluster in fallback). Note: this Rook version does not re-class the existing `.mgr`
    pool's CRUSH rule when `deviceClass` is set on `builtin-mgr` even with
    `allowDeviceClassUpdate`, so enabling the feature in practice needs `.mgr` recreated or
    otherwise re-ruled -- worth a user-doc callout.
  - After the review-fix build (idle short-circuit, per-class noout, fallback handoff): redeployed;
    the healthy idle cluster reconciles via the idle fast path (logs show "successfully reconciled
    OSD PDB controller" without the full-path "PGs are clean" line), confirming no Ceph gate/PG
    reads on idle reconciles. Default PDB intact, no errors.
  - Not demoable on this single-node cluster: cross-failure-domain blocking PDBs and
    concurrent cross-class drains (need >= 4 OSDs across >= 2 hosts) and a live node drain
    (draining the only node stops the operator) -- all covered by unit tests instead.

## Note for enablement / upgrade

On a default Rook cluster the built-in `.mgr` pool (`CephBlockPool/builtin-mgr`) ships with
an empty `deviceClass`, which keeps the cluster on the fallback path. The device-class
feature only activates once every pool declares a non-empty `deviceClass`. See
[Built-in pools and the classIsolated decision](#built-in-pools-and-the-classisolated-decision)
for the full analysis of `.mgr`, `.rgw.root`, and `.nfs`, the safety gap in the CR-spec-based
check, and the proposed CRUSH-based refinement.
- [ ] **Open decision:** make `classIsolated` (and per-class failure domain) CRUSH-based
  instead of CR-spec-based, to close the `.nfs`/directly-created-pool safety gap and the
  CR-versus-CRUSH drift. See
  [Built-in pools and the classIsolated decision](#built-in-pools-and-the-classisolated-decision).
- [ ] Update [ceph-managed-disruptionbudgets.md](design/ceph/ceph-managed-disruptionbudgets.md).

## Design goals

- **Keep existing PDB names.** Both the default `rook-ceph-osd` and the per-failure-domain blocking `rook-ceph-osd-<fdType>-<fdName>` are preserved.
- **Only selectors change.** No new PDB objects, no renames, no new field on the CephCluster CRD.
- **Fall back to today's behavior** whenever class isolation cannot be proven from the pool specs.

These constraints make the upgrade story an in-place selector update and keep the visible PDB layout familiar to operators already running Rook.

## Current behavior (one paragraph)

See [ceph-managed-disruptionbudgets.md](design/ceph/ceph-managed-disruptionbudgets.md) for the full design. Briefly: Rook creates one default PDB (`rook-ceph-osd`, `maxUnavailable=1`) covering every OSD, and during a drain replaces it with one blocking PDB per non-draining failure domain (`rook-ceph-osd-<fdType>-<fdName>`, `maxUnavailable=0`). The blocking PDB's selector matches every OSD in that failure domain regardless of device class, so a drain in any class blocks every class in every other failure domain.

## Proposed change

The PDB layout — names, `maxUnavailable`, and the rule of one default plus one blocking PDB per non-draining failure domain — stays as today. Two selector additions express device-class awareness on the existing objects.

The supporting work outside selector construction is real, though: per-class state in the ConfigMap, per-class failure-domain enumeration, per-class PG cleanliness, per-class `noout` handling, and a write-ordering contract for the reconcile (see [Reconcile invariant and update ordering](#reconcile-invariant-and-update-ordering)). The "only selectors change" claim describes the PDB layout, not the reconcile code.

### Default PDB

Name, `maxUnavailable`, and lifecycle stay as today, with one change: it is no longer deleted during a drain. Instead its selector gains a `device-class NotIn` clause that excludes classes currently in drain. Classes not being drained continue to be protected by `maxUnavailable=1`.

### Blocking PDB

Per failure domain as today. Selector gains a `device-class In` clause listing the classes draining at a different failure domain. A blocking PDB is created at failure domain `f` only if that list is non-empty.

### Fallback

If any pool in the cluster has an empty `spec.deviceClass`, isolation by class cannot be assumed (the pool's CRUSH rule may span classes). On the fallback path the class clause is omitted from both PDBs and the operator's behavior is identical to today's code — including deletion of the default PDB during a drain (without a class filter, the default PDB and a blocking PDB would otherwise double-match every OSD in the blocking PDB's failure domain).

Pool `spec.deviceClass` and the OSD pod's `device-class` label carry the same Ceph device-class names (set from `osd.DeviceClass` at [`labels.go:61`](pkg/operator/ceph/cluster/osd/labels.go)). The fallback decision based on the pool spec is therefore consistent with the labels the PDB selectors match against.

## State the operator maintains

The `rook-ceph-pdbstatemap` ConfigMap already tracks one `drainingFailureDomain` key. The new layout is one key per device class:

```
draining-failure-domain-hdd: zone-a
draining-failure-domain-ssd: zone-c
set-no-out-hdd: "true"
set-no-out-ssd: "true"
```

Absence of a key means that class has no active drain. The legacy single key `draining-failure-domain` is read on startup once for backward compatibility (see [Mid-drain upgrade](#mid-drain-upgrade)).

At most one draining failure domain is tracked per device class concurrently. This is the natural per-class generalization of today's single-drain limit (today, the singular `drainingFailureDomain` key allows at most one drain anywhere; `setPDBConfig` at [`osd.go:548-552`](pkg/operator/ceph/disruption/clusterdisruption/osd.go) picks the first failure domain when several appear). If two HDD nodes in different failure domains are drained concurrently, the second drain is queued behind the first.

## Reconcile invariant and update ordering

**Invariant.** At every instant, each OSD pod is matched by either the default PDB or by exactly one blocking PDB, never both. A pod matched by zero PDBs is allowed only when it sits in the draining failure domain of its class (intentional exposure for eviction).

This invariant matters because of a hard Kubernetes constraint. The eviction subresource ([`pkg/registry/core/pod/storage/eviction.go:224-230`](https://github.com/kubernetes/kubernetes/blob/master/pkg/registry/core/pod/storage/eviction.go)) returns HTTP 500 if a pod matches more than one PDB:

> "This pod has more than one PodDisruptionBudget, which the eviction subresource does not support."

In steady state the invariant holds by construction (the blocking `In` list and the default `NotIn` list are the same set, so the two clauses are complementary — verified across Cases 1 through 5). The risk is during reconcile transitions, where multiple non-atomic API writes can briefly produce double-match or no-match states.

**Write-ordering rule.** Always widen coverage before narrowing it. A single reconcile may move several classes at once (one class starts draining while another stops), so instead of branching per class, the apply runs the same three phases in a fixed order. Let the new draining set define the desired `NotIn` set on the default PDB and the desired `In` list on each blocking PDB.

1. **Widen blocking.** For every blocking PDB, set its `In` list to the union of its old and new lists, creating any blocking PDB the new state needs. This only adds coverage; nothing is removed yet.
2. **Update the default PDB.** Write the new `NotIn` set in a single update.
3. **Narrow blocking.** Set each blocking PDB's `In` list to its new value, deleting the PDB when its `In` list empties.

This ordering is gap-free for any mix of classes entering and leaving. Between phase 1 and phase 2, a class that just started draining is matched by both its widened blocking PDB and the not-yet-updated default PDB; between phase 2 and phase 3, a class that just stopped draining is matched by both the re-widened default PDB and the not-yet-narrowed blocking PDB. Both transient states are double-match, never no-match. A reconcile pass interrupted between any two phases leaves the cluster in steady state or a double-match state. Both are safe — HTTP 500 fails closed, so eviction never proceeds during an inconsistency.

`handleActiveDrains` and `handleInactiveDrains` ([`osd.go:343-384`](pkg/operator/ceph/disruption/clusterdisruption/osd.go)) are replaced by this three-phase apply. The previous draining set is read from the ConfigMap state; the new set is the result of `setPDBConfig`/`resetPDBConfig` for the current reconcile.

**Alternative considered.** Per-class default PDBs (e.g. `rook-ceph-osd-hdd`, `rook-ceph-osd-ssd`) would sidestep the ordering contract entirely — each pod is matched by exactly one default-or-blocking pair regardless of write order. Rejected because it violates the "keep existing PDB names" goal and forces a rename and migration that the in-place approach avoids.

## Required code changes outside selector construction

The reconcile changes are larger than the PDB layout suggests. The full list:

- **Per-class minimum failure domain.** `getMinimumFailureDomain` ([`pools.go:78`](pkg/operator/ceph/disruption/clusterdisruption/pools.go)) and `getOSDFailureDomains` ([`osd.go:436`](pkg/operator/ceph/disruption/clusterdisruption/osd.go)) compute one global value today. They become per-class so pools using different failure-domain types (HDD pools on `zone`, SSD pools on `host`) get correct per-class failure-domain enumeration.
- **Per-class state map and PDB config logic.** `setPDBConfig`, `resetPDBConfig`, and the switch in `reconcilePDBsForOSDs` ([`osd.go:277-324`](pkg/operator/ceph/disruption/clusterdisruption/osd.go)) all key off the singular `drainingFailureDomainKey`; they need per-class variants.
- **Per-class PG cleanliness.** [`IsClusterClean`](pkg/daemon/ceph/client/status.go) reports PG health globally. A per-class variant resolves the class's RADOS pools from the CRUSH map (rule `item_name` suffix after `~`) and aggregates `pg ls-by-pool` across them. This is the only genuinely new external behavior; everything else restructures existing logic. See [Per-class PG cleanliness](#per-class-pg-cleanliness).
- **`noout` handling.** `updateNoout` ([`osd.go:386-434`](pkg/operator/ceph/disruption/clusterdisruption/osd.go)) reads the singular `drainingFailureDomainKey`. Its `else` branch (line 424) actively unsets `noout` on every failure domain that is not equal to that key. With per-class draining failure domains, this fights itself on every reconcile — whichever class's draining failure domain is not the singular key has its `noout` unset. The function must be reworked to consider the union of all per-class draining failure domains. Per-class `noout` granularity (`ceph osd set-group noout zone=zone-a class=hdd`) is a follow-up; the immediate required change is just "set `noout` on the union, not unset siblings of one chosen FD".
- **`excludeOSDs` (chronically-down OSDs).** Today's `osd NotIn [ids]` clause on the default PDB lists down OSDs cluster-wide. A chronically-down HDD OSD should not suppress SSD's class-aware drain behavior; the per-class default PDB selectors keep their independent `osd NotIn` lists.
- **Write-ordering contract.** See [Reconcile invariant and update ordering](#reconcile-invariant-and-update-ordering).

## Cases

The cluster used in the examples has three zones (`a`, `b`, `c`) and two device classes (`hdd`, `ssd`). Every zone hosts OSDs of both classes. One pool `hdd-pool` has `deviceClass: hdd`, one pool `ssd-pool` has `deviceClass: ssd`.

### Case 1 — idle

No OSD down, all PGs clean.

| PDB | Selector | maxUnavailable |
| --- | --- | --- |
| `rook-ceph-osd` | `app=rook-ceph-osd` | 1 |

No blocking PDBs.

### Case 2 — one class draining

Admin drains an HDD node in zone-a. `osd-hdd-a` goes down.

| PDB | Selector | maxUnavailable |
| --- | --- | --- |
| `rook-ceph-osd` | `app=rook-ceph-osd AND device-class NotIn [hdd]` | 1 |
| `rook-ceph-osd-zone-zone-b` | `topology-location-zone=zone-b AND device-class In [hdd]` | 0 |
| `rook-ceph-osd-zone-zone-c` | `topology-location-zone=zone-c AND device-class In [hdd]` | 0 |

What this allows and blocks:
- HDD OSDs in zone-a: not matched by any PDB. Eviction proceeds.
- HDD OSDs in zone-b, zone-c: matched by blocking PDB with `maxUnavailable=0`. Blocked.
- SSD OSDs in any zone: matched by the default PDB only. A second drain affecting one SSD OSD anywhere is permitted; a third is blocked by `maxUnavailable=1`.

### Case 3 — second class also draining, different zone

While HDD-zone-a is still draining, admin starts draining an SSD node in zone-c.

| PDB | Selector | maxUnavailable |
| --- | --- | --- |
| `rook-ceph-osd` | `app=rook-ceph-osd AND device-class NotIn [hdd, ssd]` | 1 |
| `rook-ceph-osd-zone-zone-a` | `topology-location-zone=zone-a AND device-class In [ssd]` | 0 |
| `rook-ceph-osd-zone-zone-b` | `topology-location-zone=zone-b AND device-class In [hdd, ssd]` | 0 |
| `rook-ceph-osd-zone-zone-c` | `topology-location-zone=zone-c AND device-class In [hdd]` | 0 |

The default PDB selector now matches nothing (no class is left undrained). That is fine — drain protection is fully provided by the blocking PDBs.

The zone-a blocking PDB now exists (it did not in Case 2) because SSD has a drain elsewhere and needs to be blocked here. HDD in zone-a is still free because the selector is `In [ssd]`, not `In [hdd]`.

### Case 4 — two classes draining the same zone

A single mixed-class node in zone-a is drained, taking both `osd-hdd-a` and `osd-ssd-a` down.

| PDB | Selector | maxUnavailable |
| --- | --- | --- |
| `rook-ceph-osd` | `app=rook-ceph-osd AND device-class NotIn [hdd, ssd]` | 1 |
| `rook-ceph-osd-zone-zone-b` | `topology-location-zone=zone-b AND device-class In [hdd, ssd]` | 0 |
| `rook-ceph-osd-zone-zone-c` | `topology-location-zone=zone-c AND device-class In [hdd, ssd]` | 0 |

The zone-a PDB is absent because every class is draining there — nothing remains to block.

In Cases 3 and 4 the default PDB selector matches zero OSD pods. The Kubernetes disruption controller ([`pkg/controller/disruption/disruption.go:1004`](https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/disruption/disruption.go)) reports `DisruptionsAllowed=0` for any PDB with no expected pods, so `requeuePDBController` ([`osd.go:573-589`](pkg/operator/ceph/disruption/clusterdisruption/osd.go)) requeues every 30 seconds for the duration of an all-classes drain. This is a behavior change: today's code uses default-PDB-deletion as the in-drain signal, and that signal is no longer available. The 30s requeue is not a bug, but the operator's logs will be busier during a multi-class drain than they are today.

### Case 5 — one class completes, the other still draining

Continuing from Case 3: HDD finishes draining and HDD PGs become clean, SSD is still rebalancing.

| PDB | Selector | maxUnavailable |
| --- | --- | --- |
| `rook-ceph-osd` | `app=rook-ceph-osd AND device-class NotIn [ssd]` | 1 |
| `rook-ceph-osd-zone-zone-a` | `topology-location-zone=zone-a AND device-class In [ssd]` | 0 |
| `rook-ceph-osd-zone-zone-b` | `topology-location-zone=zone-b AND device-class In [ssd]` | 0 |

The zone-c PDB is deleted (no class needs blocking there anymore — SSD is the draining FD here, and HDD is no longer draining anywhere). The zone-a and zone-b PDBs are updated in place to drop HDD from their `In` lists.

The transition `Case 3 → Case 5` requires checking PG cleanliness *per class* (HDD PGs are clean, SSD PGs are not). See [Per-class PG cleanliness](#per-class-pg-cleanliness).

## Edge cases

This section lists the cases that shaped the design. Each entry says what happens and why.

### Unclassified pool

A pool with empty `spec.deviceClass` uses a CRUSH rule that may span device classes. There is no class-based isolation to exploit, and applying a class filter to PDBs would create an unprotected gap.

`processPools` checks whether every pool has a non-empty `deviceClass`. If any pool does not, the device class clause is omitted from both PDBs and the operator's behavior is identical to today's code. Mixed-class clusters that share pools across classes lose no protection and gain no parallelism.

### Different failure-domain type per class

`hdd-pool` may use failure domain `zone` while `ssd-pool` uses `host`. The minimum failure domain is computed per class instead of globally. PDB names embed the failure domain type (`rook-ceph-osd-zone-zone-a` for HDD, `rook-ceph-osd-host-node-x` for SSD), so the two classes' PDBs live in different objects and never collide.

### Class declared in pool spec but no OSDs of that class

A pool may declare `deviceClass: nvme` before any NVMe OSDs are added. Nothing matches the class selector. Default and blocking PDB selectors that include the class still resolve to no pods. No action needed.

### Per-class PG cleanliness

The current code uses [`IsClusterClean`](pkg/daemon/ceph/client/status.go) which reports PG health globally. With concurrent drains in different classes, one class's PGs may be clean while the other class's PGs are still recovering, and the operator must unblock the healthy class without waiting for the other.

A per-class variant resolves the class's pools entirely from Ceph, so it does not depend on translating CR pool names into RADOS pool names. The mapping uses the CRUSH map (`osd crush dump`): each CRUSH rule's `take` step carries an `item_name` of the form `default~ssd` for a class-isolated rule (plain `default` means the rule spans classes, which only occurs on the fallback path). Joining the pool-to-rule mapping with rule-to-class gives the set of RADOS pools per device class. The class is "clean" when every PG of every one of its pools passes the configured `pgHealthyRegex` (`pg ls-by-pool <pool>` per pool, then aggregate). Listed in [Required code changes](#required-code-changes-outside-selector-construction) as the only genuinely new external behavior; everything else there restructures existing logic.

### OSD with an empty `device-class` label value

Rook sets the `device-class` label unconditionally at [`labels.go:61`](pkg/operator/ceph/cluster/osd/labels.go) — `labels[deviceClass] = osd.DeviceClass`. The key is always present on the pod; the *value* can be the empty string when `osd.DeviceClass` is unset (an OSD that has not been tagged with a CRUSH device class).

This case is safe by construction. An empty value passes `device-class NotIn [hdd, ssd]` (empty is not a member of the list), so the OSD remains matched by the default PDB and protected at `maxUnavailable=1`. It does not match any blocking PDB's `device-class In` clause either. Such an OSD is treated as "uncategorized" — protected by the default PDB, never participates in a class-aware drain.

### Mid-drain upgrade

A cluster upgraded while a drain is in progress has:
- pre-existing blocking PDBs `rook-ceph-osd-<fdType>-<fdName>` with selectors lacking the class clause,
- a ConfigMap entry `draining-failure-domain: zone-x` (no per-class keys),
- a deleted default PDB.

On first reconcile, the new code detects the legacy key, infers "drain affecting unknown classes", and treats this drain as global (no class filter applied). The existing blocking PDBs are updated in place with no `In` clause, the default PDB is recreated with no `NotIn` clause. Once that drain completes and the per-class keys take over, subsequent drains use the class-aware behavior.

No PDB renames, no create-before-delete dance, no one-shot legacy cleanup.

### Chronically-down OSDs (`osd NotIn`)

The default PDB already supports excluding chronically-down OSDs via `osd NotIn [ids]`. The new `device-class NotIn` is an additional `matchExpression` on the same selector, computed independently. Both clauses coexist.

### Downgrade

The old operator does not understand the `device-class` clauses but still respects the PDBs as Kubernetes objects. On downgrade during an active drain, the old code's "create-if-not-exists" path leaves the new-shape PDBs in place, which means SSD in non-draining failure domains becomes unprotected while HDD is draining. Downgrade mid-drain is not supported. Downgrade in the idle state is safe because the new-shape default PDB carries no class clause when no class is draining.

## Built-in pools and the classIsolated decision

The fallback hinges on `processPools` deciding whether every pool isolates a device class.
`processPools` builds that decision from CR specs only: it lists `CephBlockPool`,
`CephFilesystem` (metadata plus data), and `CephObjectStore` (metadata plus data). Two facts
about the cluster's built-in pools complicate this.

What the built-in pools actually are:

- `.mgr` is a real `CephBlockPool` CR (`builtin-mgr`). Its `deviceClass` is empty by default
  but is settable on the CR, and `processPools` does see it. A default cluster therefore stays
  on the fallback path until `builtin-mgr` (and every other pool) is classified.
- `.rgw.root` and the other RGW service pools are created from the object store's
  `spec.metadataPool` and `spec.dataPool` ([`objectstore.go:814`](pkg/operator/ceph/object/objectstore.go)
  appends `rootPool` to the metadata pools, then `createSimilarPools` applies the metadata
  `PoolSpec`). They inherit that spec's `deviceClass`, and `processPools` already examines those
  specs, so RGW does not introduce a hidden pool. It is also empty by default.
- `.nfs` (CephNFS) is created with a bare `osd pool create`
  ([`nfs.go:330`](pkg/operator/ceph/nfs/nfs.go)) with no device class, so it uses the default
  rule that spans all classes. `processPools` does not list `CephNFS` at all.

This surfaces two problems with deciding `classIsolated` from CR specs:

1. **Pools the CR scan never sees.** `.nfs` spans device classes and is invisible to
   `processPools`. With a `CephNFS` present, `classIsolated` could be true while `.nfs` actually
   spans classes. Draining a whole class could take `.nfs` PGs down, and the per-class
   cleanliness check excludes class-spanning pools, so a class could unblock while `.nfs` is
   still degraded. That is a data-availability gap. Any future directly-created pool has the
   same exposure.
2. **CR-versus-CRUSH drift.** Setting `builtin-mgr.spec.deviceClass=hdd` does not always update
   the live `.mgr` CRUSH rule (observed on the Lima cluster: the CR said `hdd` while the rule
   still spanned classes). A CR-based check then reports "isolated" when CRUSH says "spanning."

Failure-domain note: `.mgr` and `.rgw.root` commonly use a different (often `host`) failure
domain than data pools (`zone`) and need not cover every zone. The per-class minimum failure
domain already handles differing failure-domain types per class, and blocking PDBs are
enumerated from where each class's OSDs live (pod labels), not from pool placement, so a pool
that does not span all zones does not break enumeration. An infra pool that spans device
*classes* is exactly the case the fallback must catch.

**Decision (accepted): the classIsolated determination is CRUSH-based, not CR-based.** Decide
`classIsolated` from the actual CRUSH rules of all existing Ceph pools rather than from CR
specs. The map is read in two bounded calls -- `osd crush dump` (rule to class via the `take`
step's `item_name`, and rule to failure domain via the `chooseleaf` step's `type`) joined to
`osd pool ls detail` (pool to rule id). The rule: if any existing pool maps to the empty class
(a class-spanning rule), fall back. This closes the `.nfs` / directly-created-pool gap (PO1),
removes the CR-versus-CRUSH drift, makes the isolation decision and the per-class cleanliness
check use a single source of truth (so P1's divergence cannot occur), reads the per-class
failure domain from CRUSH instead of the CR `failureDomain` field, and replaces the previous
N+1 `osd pool get` calls (P2) with two bounded queries. Because the corrected gate now notices
spanning pools that the CR scan missed, it can flip a cluster back to fallback mid-drain, so it
ships together with the R1 handoff: the fallback path deletes any class-scoped blocking PDB
(one carrying a `device-class In` clause) and clears the per-class ConfigMap keys, and the
per-class keys are namespaced under a `device-class/` prefix so that cleanup is unambiguous
(D4). Per-class PG cleanliness is queried only for classes that have a down OSD or an active
drain, not on every idle reconcile (P2).

## Out of scope for this change

- **Per-class `noout` granularity.** Once the union fix from [Required code changes](#required-code-changes-outside-selector-construction) is in place, `noout` is still set on the entire failure-domain CRUSH bucket of each draining class. The natural finer-grained form is `ceph osd set-group noout zone=zone-a class=hdd`. Deferred as a follow-up. Until then, the only consequence is that data migration is paused for non-draining classes inside a draining failure domain for the maintenance window — conservative and safe.
- **A CRD opt-in flag.** The fallback for unclassified pools means the new behavior cannot regress existing clusters, so no flag is proposed. If reviewers prefer an explicit gate, add `spec.disruptionManagement.osdDisruptionBudgetByDeviceClass: bool` and branch in `reconcilePDBsForOSDs`. No other change.

## Backward compatibility summary

| Pre-upgrade state | Behavior |
| --- | --- |
| Idle, single class, pool has `deviceClass` set | Default PDB updated in place to include `device-class` clause when next drain starts. Identical observable behavior until a drain. |
| Idle, no `deviceClass` on any pool | Fallback path. No selector changes. No observable change. |
| Mid-drain | Legacy ConfigMap key triggers no-class-filter path for that drain. Existing PDBs updated in place to match the no-filter selectors. Next drain uses class-aware path. |
| Default PDB has `osd NotIn [ids]` | Preserved. The new `device-class NotIn` is appended as a separate `matchExpression`. |
