# Matching service — independent repository

CEL + Roaring Bitmap, single shard. This implements the agreed matching flow; it does not claim hundred-million-object production capacity.

## API and rule contract

- `POST /v1/match`: filter strategies by scene/type, evaluate `when` with CEL, bind context references in `query`, execute bitmap operations, check candidate eligibility, deduplicate and sort.
- `PUT /v1/config`: bearer-authenticated **full snapshot replacement**, with increasing config version. Build/validate, persist, then switch the active engine atomically. Invalid/stale/failed writes leave the current version active. No per-candidate mutation API yet.
- `GET /healthz`, `GET /readyz`: health and current snapshot version.
- Complete working payloads: `examples/config.json`, `examples/request.json`.
- Business IDs are canonical positive signed-bigint decimal **strings**, avoiding JSON integer precision loss. Identity is `(type, id)`.
- Dense uint32 local IDs are assigned per complete snapshot. A local ID is meaningful only with `shard_id` and response snapshot `version`. Index and mapping always switch together; no in-place ID reuse.
- Schemas: flat candidate attributes, dotted context paths; types `keyword`, `keyword[]`, `number`, `boolean`. Candidate indexed fields are explicit. Numbers must have magnitude <= 2^53-1; exact minor-unit amounts must stay within that safe integer range. Strings are case-sensitive canonical business values; arbitrary IDs are not lowercased.
- Rule nodes: exactly one of `all`, `any`, `not`, or leaf (`field`, `op`, `value`). Value is either `literal` or `ref`. Empty groups/operand arrays are rejected. Depth <=16, <=256 nodes per rule, <=256 strings per array.
- Operators: `eq/ne` scalar; `in` keyword against keyword array; `contains_any/contains_all` keyword array; `gt/gte/lt/lte` numeric; `exists` without operand.
- Missing means UNKNOWN, including under NOT; only TRUE matches. `exists` explicitly checks presence. Bitmap evaluator tracks both true and false sets; NOT swaps them. CEL compiles guarded true/false expressions to preserve the same semantics. Invalid context types/unknown fields reject the request. CEL evaluation errors return 422.
- All matched strategies execute. Rank by matching-strategy count descending, then numeric business ID ascending. Response retains matched strategy IDs, candidate business version, config version, local ID and shard ID. Detailed condition traces are deferred.
- Limit defaults to 20, max 1000; `total` is after eligibility and before limit.
- Errors: 400 malformed JSON/config, 401 unauthorized publish, 409 stale config version, 422 invalid match request/evaluation failure, 500 persistence failure. Body limit 32 MiB.

## Deploy and use

From this repository root (with the sibling local-platform-lab checkout):

```sh
make -C ../local-platform-lab deploy-matching-service
make -C ../local-platform-lab verify-matching-service
kubectl --context kind-local-platform -n matching-service port-forward service/matching-service 18088:8080
```

In another terminal:

```sh
curl -s http://localhost:18088/readyz
curl -s http://localhost:18088/v1/match -H 'Content-Type: application/json' --data-binary @examples/request.json
```

Deployment runs tests/vet, builds and pushes an image to localhost:5001, creates an admin Secret if absent, commits only matching-service GitOps files, pushes to the **cluster-local** Git server without force, and reconciles via Argo CD. It does not push the outer source repository to GitHub. Secret values are never committed. Management writes require `Authorization: Bearer <ADMIN_TOKEN>`.

The smoke script assumes the bundled sample dataset. After loading business data, update its assertions. Cluster commands pin `kind-local-platform`. ClusterIP service, no public gateway route, no BFF modification. The match endpoint has no authentication and is intended only for this private lab.

## Persistence and rollout

One replica with Recreate strategy; short downtime during restart/update. Non-root container, 1 GiB PVC, `/data/snapshot.json`. Restart loads persisted data; image examples seed only an empty volume. Do not scale replicas: local writers/state are not distributed.

Data rollback: publish an earlier full payload with a **higher** config version. Code rollback: revert the image tag in GitOps and reconcile; preserve compatibility with the current snapshot format. Do not delete the PVC during routine rollbacks.

## Validation and production boundary

Tests cover CEL selection, bitmap operators, missing/NOT, inactive candidates, deduplication, eligibility, invalid schema/rules/IDs/types, owned build input, concurrent reads, authenticated atomic publishing and persisted reload. A deterministic 2,000-object scan independently checks bitmap results.

Full snapshot builds hold old and new indexes concurrently. Startup recompiles JSON source. Numeric ranges iterate distinct values and union bitmaps; ranking/eligibility examine the complete recall set. Before large-scale production: incremental durable ingestion, stable allocation, segmented/range indexes, bounded recall/ranking, compiled-rule reuse, distributed routing/merge, metrics and capacity/recovery tests are required. There is no scale or latency guarantee for hundreds of millions of candidates.

## Repository boundary and review fixes

This repository owns the Go service, rules, candidate schema, tests, examples and Dockerfile. The sibling `local-platform-lab` repository owns Kubernetes manifests, Argo CD and deployment verification. Its scripts default to `../matching-service`; set `MATCHING_SERVICE_DIR` to use another checkout. No GitHub remote is configured automatically.

Review fixes: decoded JSON literals are bound as CEL program data (not source syntax); numeric index keys normalize negative zero; server lifecycle waits for active-request drain; platform release verification requires the intended GitOps commit and image plus healthy, observed replica state.
