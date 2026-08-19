# provisioning — node health reporting

`pkg/provisioning` reports AKS node provisioning health to the Azure wireserver. It is a Go
port of [azure-init](https://github.com/Azure/azure-init)'s health reporting
(`libazureinit/src/health.rs`, `report.rs`, `error.rs`) and replaces the earlier Python
`cloud_init_report_ready.py` workaround.

The package exposes a Go API (`ReportReady` / `ReportFailure`) and ships a CLI on the VHD:

- **`provisioning-report`** ([cmd/provisioning-report](../../cmd/provisioning-report)) is a thin
  standalone CLI over the API. It ships on the VHD at `/opt/azure/containers/provisioning-report`
  and can be invoked from a boothook / `runcmd` or manually to report `Ready` on success or
  `NotReady` / `ProvisioningFailed` on failure.
- **Go API** — `provisioning.ReportReady` / `ReportFailure`, for any in-process Go caller (for
  example, if `aks-node-controller` is later wired to report automatically; see
  [reporting-rollout.md](reporting-rollout.md)).

## Protocol

Unlike the legacy WALinuxAgent / cloud-init protocol (`GET /machine/?comp=goalstate` followed by
an XML `POST /machine?comp=health`), this uses the modern JSON protocol azure-init uses: a single
`POST` of a small JSON document to `http://168.63.129.16/provisioning/health`. There is **no
GoalState fetch and no XML** — no ContainerId / InstanceId / Incarnation is required.

Request headers:

- `User-Agent: AKS-CSE`
- `x-ms-guest-agent-name: AKS-CSE`
- `Content-Type: application/json`

Ready body:

```json
{"state":"Ready"}
```

Failure body (the `description` is the pipe-delimited encoded report):

```json
{
  "state": "NotReady",
  "details": {
    "subStatus": "ProvisioningFailed",
    "description": "result=error|reason=<reason>|agent=AKS-CSE|<supporting-data>|pps_type=None|vm_id=<uuid>|timestamp=<rfc3339>|documentation_url=https://aka.ms/linuxprovisioningerror"
  }
}
```

### Retry behavior

Each report is retried until it is delivered or the total retry budget elapses
(`--total-retry-timeout`, default 20m, matching azure-init):

- **Success** on any `2xx`.
- **Retry** on `429`, `500`, `503` and transport/connection errors.
- **Fatal** (no retry) on any other non-`2xx` (e.g. `400`, `401`, `403`, `404`).

### VM ID

The `vm_id` field is read from `/sys/class/dmi/id/product_uuid`, falling back to
`dmidecode -s system-uuid`, then to the all-zero UUID. It requires no privileges and works
pre-network.

### KVP diagnostics

Alongside the wireserver report, the encoded report is written to the Hyper-V guest KVP pool file
(`/var/lib/hyperv/.kvp_pool_1`) under the `PROVISIONING_REPORT` key, together with a `dmesg`
snapshot, so the Azure/Hyper-V host can read provisioning telemetry. It uses azure-init's KVP
record format with host-readable, upsert-based values:

- Records use the fixed-width 512-byte key + 2048-byte value format (zero-padded UTF-8), with a
  host-readable value limit of 1022 bytes per record. A report that fits is written verbatim under
  `PROVISIONING_REPORT`; a larger report is split on UTF-8 boundaries into indexed chunk records
  `PROVISIONING_REPORT|0`, `PROVISIONING_REPORT|1`, ..., which a reader reassembles by concatenating
  the chunk values in index order (cloud-init's key-suffix chunk indexing, without its JSON value
  envelope).
- `PROVISIONING_REPORT` and `dmesg` use upsert semantics, so repeated calls leave one current value
  for each key (replacing any prior single or chunked records) while preserving unrelated KVP records.
- Stale checking, truncation, and rewriting happen while holding both `flock` and `fcntl` locks,
  coordinating with cloud-init / azure-init and `hv_kvp_daemon`.
- KVP writes are **best-effort**: any failure is logged and never blocks or fails the wireserver
  report. Disable them with `--disable-kvp`.

## CLI usage

```text
provisioning-report report-ready   [--endpoint URL] [--agent NAME] [--total-retry-timeout DUR] [--disable-kvp] [--kvp-path PATH]
provisioning-report report-failure [--endpoint URL] [--agent NAME] [--total-retry-timeout DUR] [--disable-kvp] [--kvp-path PATH] \
                                   --reason TEXT [--documentation-url URL] \
                                   [--supporting-data key=value ...]
```

| Flag | Default | Description |
| --- | --- | --- |
| `--endpoint` | `http://168.63.129.16/provisioning/health` | Wireserver provisioning-health endpoint. |
| `--agent` | `AKS-CSE` | Reporting agent identifier (report `agent=` field and request headers). |
| `--total-retry-timeout` | `20m` | Total budget for delivering the report. |
| `--disable-kvp` | `false` | Do not write the report and `dmesg` to the Hyper-V KVP pool. |
| `--kvp-path` | `/var/lib/hyperv/.kvp_pool_1` | Override the Hyper-V KVP pool file path. |
| `--reason` / `--description` | required | Human-readable failure reason (report-failure only). |
| `--documentation-url` | `https://aka.ms/linuxprovisioningerror` | Documentation URL in the failure report. |
| `--supporting-data` | — | Extra `key=value` context, repeatable (report-failure only). |

### Exit codes

The commands are **fail-closed**: a non-zero exit code means the report could not be delivered
within the retry budget (or an argument was invalid). Successfully reporting a *failure* exits
`0` — the report itself was delivered.

### Examples

```bash
provisioning-report report-ready
provisioning-report report-failure --reason "CSE failed with exit code 42" --supporting-data exit_code=42
```

## How reporting runs

**On a node (`provisioning-report`).** The CLI ships on the VHD at
`/opt/azure/containers/provisioning-report` and is invoked to report `Ready` / `Failure` — for
example from a cloud-init boothook / `runcmd`, or manually.

**Testing (`provisioning-report` + mock wireserver).** Point the CLI at a mock endpoint to verify
reporting without touching the real fabric:

```bash
# terminal 1: a mock wireserver that records what it receives
go run ./cmd/mockwireserver --addr 127.0.0.1:8899

# terminal 2: drive the CLI at it
provisioning-report report-ready --endpoint http://127.0.0.1:8899/provisioning/health
```

For automated tests, `provisioningtest.NewMockWireserver()` provides an in-process recording
wireserver; the package's integration tests assert both the HTTP report and the KVP records.

## Scope

This package reports provisioning health to the wireserver and writes the encoded report plus a
`dmesg` snapshot to the Hyper-V KVP pool. It does **not** decide *when* or *whether* to report —
that is the caller's responsibility (the `provisioning-report` CLI invoked from a boothook /
`runcmd`, or a future automatic integration in `aks-node-controller`). Coordinating this with
cloud-init's own ready report — so a node is not reported ready twice or prematurely — is described
in [reporting-rollout.md](reporting-rollout.md).
