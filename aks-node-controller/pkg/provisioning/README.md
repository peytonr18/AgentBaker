# provisioning — node health reporting

`pkg/provisioning` reports AKS node provisioning health to the Azure wireserver. It is a Go
port of [azure-init](https://github.com/Azure/azure-init)'s health reporting
(`libazureinit/src/health.rs`, `report.rs`, `error.rs`) and replaces the earlier Python
`cloud_init_report_ready.py` workaround.

The `aks-node-controller` binary exposes it as two subcommands — `report-ready` and
`report-failure` — that can be invoked independently of the normal provisioning flow (for
example, baked into an image's `runcmd`).

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

## CLI usage

```text
aks-node-controller report-ready   [--endpoint URL] [--agent NAME] [--total-retry-timeout DUR]
aks-node-controller report-failure [--endpoint URL] [--agent NAME] [--total-retry-timeout DUR] \
                                   [--reason TEXT] [--documentation-url URL] \
                                   [--supporting-data key=value ...]
```

| Flag | Default | Description |
| --- | --- | --- |
| `--endpoint` | `http://168.63.129.16/provisioning/health` | Wireserver provisioning-health endpoint. |
| `--agent` | `AKS-CSE` | Reporting agent identifier (report `agent=` field and request headers). |
| `--total-retry-timeout` | `20m` | Total budget for delivering the report. |
| `--reason` / `--description` | `""` | Human-readable failure reason (report-failure only). |
| `--documentation-url` | `https://aka.ms/linuxprovisioningerror` | Documentation URL in the failure report. |
| `--supporting-data` | — | Extra `key=value` context, repeatable (report-failure only). |

### Exit codes

The commands are **fail-closed**: a non-zero exit code means the report could not be delivered
within the retry budget (or an argument was invalid). Successfully reporting a *failure* exits
`0` — the report itself was delivered.

### Examples

Report success / failure directly:

```bash
aks-node-controller report-ready
aks-node-controller report-failure --reason "CSE failed with exit code 42" --supporting-data exit_code=42
```

## Baking into an image (runcmd)

The commands can be invoked from cloud-init `runcmd`, so an image can report its own health
without any custom data. Assuming the binary is installed at
`/opt/azure/containers/aks-node-controller` (its standard VHD location):

Report success:

```yaml
#cloud-config
runcmd:
  - /opt/azure/containers/aks-node-controller report-ready
```

Report failure:

```yaml
#cloud-config
runcmd:
  - /opt/azure/containers/aks-node-controller report-failure --reason "provisioning failed in runcmd"
```

## Scope

This package covers the wireserver health report only. Hyper-V KVP persistence of the encoded
report (`PROVISIONING_REPORT`) and `dmesg` diagnostics — azure-init's KVP path — are a planned
follow-up, as is honoring the `experimental_skip_ready_report` marker.
