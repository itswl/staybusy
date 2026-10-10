# staybusy

Put a controlled, predictable load on a machine. Single file, no dependencies,
static binary, 2.2 MB image.

```bash
docker run -d --name staybusy --restart always --memory 64m ghcr.io/itswl/staybusy
```

The image's `CMD` is `-cpu 25`, so with no arguments it holds 25% of the machine's
CPU. The bare binary defaults `-cpu` to `0` and just prints help — **the 25% comes
from the image, not from the program.**

Three resources, each off by default and independently controllable:

| resource | what it does |
|---|---|
| CPU | duty-cycled busy loop, continuous or pulsed |
| memory | anonymous pages, allocated and written so they are really resident |
| network | sustained transfers, either direction or both at once |

Handy for exercising autoscaling rules, validating monitoring thresholds and
alerts, reproducing resource contention, and smoke-testing capacity limits.

---

## Flags

| flag | default | meaning |
|---|---|---|
| `-cpu` | `0` (off)<br>image `CMD` uses `25` | percentage of the **whole machine**, spread over every core |
| `-cores` | `0` (all) | how many cores to load |
| `-per-core` | `0` (off) | percentage of **each** loaded core; takes precedence over `-cpu` |
| `-duty` | `100` | percentage of time spent busy. `100` = continuous, `10` = 6 min/hour |
| `-cycle` | `1h` | length of one duty cycle; applies when `-duty < 100` |
| `-mem` | `0` (off) | memory to hold: `512M` / `1.5G` / `1024K`; a bare number means GiB |
| `-net-down` | `0` (off) | sustained download rate in Mbps |
| `-net-up` | `0` (off) | sustained upload rate in Mbps; runs alongside `-net-down` |
| `-net-down-url` | Cloudflare | download source |
| `-net-up-url` | Cloudflare | upload target |
| `-version` | | print the version and exit |

## CPU

### Two ways to ask for load

```bash
staybusy -cpu 25                  # 25% of the whole machine, spread over every core
staybusy -cores 2 -per-core 50    # exactly 2 cores at 50% each, whatever the machine has
```

`-cpu` is the portable one: it means the same thing on any core count, because
utilisation is averaged across cores. That averaging is also why the load has to
be spread — one core at 25% is only 6.25% of a 4-core box, a quarter of what was
asked for.

`-cores` with `-per-core` is the literal one, for when you want a specific number
of busy cores rather than a machine-wide figure. `-per-core` wins if both are
given, and `-cores` is capped at the core count.

They compose: `-cpu 15 -cores 2` concentrates a machine-wide target onto fewer
cores, raising each to 82%. When the target is out of reach for that many cores it
says so rather than quietly falling short:

```
$ staybusy -cpu 25 -cores 2       # on an 11-core machine
cpu: 25% of the machine asked for, but 2 of 11 cores can only reach 18% - running them flat out
```

### Continuous or pulsed

| mode | flags | average cost |
|---|---|---|
| continuous (default) | `-cpu 25` | 25% |
| pulsed | `-cpu 40 -duty 10` | **4%** |
| in between | `-cpu 40 -duty 30` | 12% |

Pulsing costs a fraction of continuous load while still putting samples at the
burst level:

```
-cpu 40 -duty 10 -cycle 1h   =>  6 minutes busy per hour at 40% of the machine
  10% of samples read 40%, the rest read whatever the machine was doing anyway
  average cost = 40% x 10% = 4%
```

That is the right shape when what you care about is a **high percentile** — the
90th or 95th, say — rather than a mean. It is the wrong shape when the mean is
what matters: 4% average is 4%, however it is distributed. Continuous is the
default because it satisfies either reading.

Measured on a single-core machine, process-level, with nothing else competing:

```
-cpu 25                    -> 24%          -cpu 80  -> 78%
-cores 1 -per-core 50      -> 49%
-cpu 70 -duty 20 -cycle 20s -> 68% while busy, 0% between bursts
-cpu 50 -duty 25           -> 12% averaged over full cycles (12.5% predicted)
```

> Duty-cycled load does not stack. Two processes each asking for 25% on one core
> still total about 25%, not 50% — they contend rather than pile up. Convenient
> (they cannot starve real work) but it does mean a busy machine will report less
> than you asked for.

## Memory

A reserve is always left to the system and the request is clamped to fit —
holding less is better than wedging the machine. On a host the reserve is 512
MiB; inside a container it scales down to a quarter of the cgroup limit, so a
small limit does not hand most of its budget away:

```
$ staybusy -mem 5G                             # host has 4.74 GiB available
mem: 5.00 GiB requested, holding 4.24 GiB instead (host has 4.74 GiB available, less a 512 MiB reserve)

$ docker run --memory 1g staybusy -mem 512M    # reserve is a quarter of 1 GiB
mem: holding 512 MiB

$ docker run --memory 64m staybusy -mem 512M   # reserve is a quarter of 64 MiB
mem: only 61 MiB available, less than 64 MiB once the 16 MiB reserve is kept - skipping
```

**Inside a container the cgroup limit wins.** `/proc/meminfo` describes the host,
so consulting only that lets `docker run --memory 64m ... -mem 512M` walk past the
check and get OOM-killed by the kernel (exit 137) — with nothing in the log but a
line claiming the allocation succeeded. The limit is read from
`/sys/fs/cgroup/memory.max` (v2) or `memory.limit_in_bytes` (v1), and the smaller
of the two budgets is used.

Held memory is released when the process exits; the kernel reclaims it.

## Network

Both directions, independently, in Mbps. They run at the same time:

```bash
staybusy -net-down 20              # download only
staybusy -net-up 15                # upload only
staybusy -net-down 20 -net-up 10   # both at once
```

Off by default, since this is the one dimension that costs real bandwidth — the
log line states the daily volume up front, and every five minutes reports the rate
actually achieved.

**Pacing is a token bucket.** Every read is charged against a byte-rate bucket, so
the rate tracks the target continuously instead of bursting a whole block out at
line speed and sleeping off the difference afterwards. The bucket holds at most
200 ms of headroom (64 KiB to 4 MiB), so the worst short-window overshoot is a
fraction of a block rather than the block itself.

The upload buffer (8 MiB) is allocated on first use, so a run without `-net-up`
does not carry it.

## Docker

The image is built `FROM scratch`. A static binary needs no libc, no shell and no
runtime, so the image holds one executable and a CA bundle, and runs as
`nobody` (65534). Published for `linux/amd64` and `linux/arm64`.

```bash
# continuous 25%
docker run -d --name staybusy --restart always --memory 64m ghcr.io/itswl/staybusy
# pulsed
docker run -d --name staybusy --restart always --memory 64m ghcr.io/itswl/staybusy -cpu 40 -duty 10
# both network directions
docker run -d --name staybusy --restart always --memory 64m ghcr.io/itswl/staybusy -net-down 20 -net-up 10
# or use the compose file in this repo
docker compose up -d
```

Resident set: **2.6 MiB** idle, **13 MiB** with `-net-up` in play, plus whatever
`-mem` asks for. `--memory 64m` is a seatbelt — roughly 25x headroom on the base
figure, and it means a future bug cannot take the host down with it. Raise the
limit when using `-mem`, or the allocation is clamped to whatever the cgroup
allows: it will not crash, but it will hold less than you asked for.

> With `--restart always`, `docker stop` is undone immediately by the daemon,
> which reads as "stopped but still running". To actually stop it:
> `docker update --restart=no staybusy && docker stop staybusy`.

Releases carry prebuilt binaries for linux and darwin on amd64 and arm64, with a
`SHA256SUMS` alongside them. Tagging a commit `v*` builds and publishes both the
binaries and the image.

Building it yourself:

```bash
go build -o staybusy .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o staybusy .
docker buildx build --platform linux/amd64,linux/arm64 -t <user>/staybusy --push .
```

## Implementation notes

Each of these cost a debugging session:

- **Allocated memory has to be written.** An untouched `make()` only reserves
  address space; the kernel never backs it with physical pages, so the allocation
  is invisible in `/proc/meminfo` and does nothing.
- **A container cannot trust `/proc/meminfo`.** It describes the host, not the
  cgroup limit — see the memory section above.
- **The main goroutine cannot block on `select {}`.** With only `-mem` set, the
  allocating goroutine finishes and leaves nothing runnable, which the runtime
  reports as a deadlock and panics on. A pending timer keeps the process off that
  path, and the same select turns SIGTERM — what `docker stop` sends — into a
  clean exit.
- **A byte-rate bucket must cap what it charges.** Charging a read larger than the
  bucket would wait for tokens that can never accrue and hang the transfer, so the
  charge is capped at one bucketful.
- **A fixed memory reserve is wrong inside a container.** 512 MiB is a sane
  cushion on a host and half of a 1 GiB cgroup, so the reserve scales to a quarter
  of the budget.
- **A duty cycle with a zero-length phase respawns without bound.** `-cycle 0`, or
  a cycle shorter than one duty period, made the pulse loop spin up a fresh set of
  workers on every iteration with no sleep between; the cycle is now validated
  positive and the busy leg floored.
- **`-cores` over the core count used to zero the load.** Integer division spread
  `-cpu 5 -cores 200` over 200 workers, gave each 0%, and ran nothing. The worker
  count is clamped to the cores, as the docs always claimed.
- **CPU load is a duty cycle, not a spin.** Busy for N% of each 100 ms period,
  then sleep, with `runtime.LockOSThread()` pinning the goroutine so the scheduler
  cannot move it and skew the ratio.
- **HTTP status has to be checked.** The default download endpoint answers 403
  with a 1-byte body above 10 MB; treating that as a successful transfer would
  leave the network dimension quietly doing nothing.
- **Measuring this tool needs an idle machine.** Machine-wide readings include
  everything else running, and on a contended core the duty cycle delivers less
  than its nominal target. Process-level counters on an otherwise idle box are the
  only way to see what it actually produces.
