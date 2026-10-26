#!/usr/bin/env python3
"""Run isolated, production-stack uplink simulations without network privileges."""
import argparse
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]

CASES = [
    ("slow-64k", dict(SIZE_KB=16, RATE=8000, DELAY_MS=100, QUEUE_BYTES=32768)),
    ("slow-256k", dict(SIZE_KB=64, RATE=32000, DELAY_MS=150, QUEUE_BYTES=32768)),
    ("slow-1m", dict(SIZE_KB=256, RATE=125000, DELAY_MS=50)),
    ("regression-static", dict(SIZE_MB=4, DELAY_MS=100, BBR=0, FAIR=0, SERVER_FAIR=0, QUEUE_BYTES=4194304, MIN_POST_MBPS=5)),
    ("moderate-10m", dict(SIZE_MB=2, DELAY_MS=100, QUEUE_BYTES=4194304, MIN_UPLOAD_MBPS=2)),
    ("fast-100m", dict(SIZE_MB=4, RATE=12500000, DELAY_MS=10, QUEUE_BYTES=1048576, SERVER_WINDOW=65535, MTU=1400, MIN_UPLOAD_MBPS=10, MIN_POST_MBPS=20)),
    ("fast-500m", dict(SIZE_MB=8, RATE=62500000, DELAY_MS=10, QUEUE_BYTES=4194304, SERVER_WINDOW=65535, MTU=1400, MIN_UPLOAD_MBPS=10, MIN_POST_MBPS=20)),
    ("fast-2g", dict(SIZE_MB=8, RATE=250000000, DELAY_MS=10, QUEUE_BYTES=16777216, SERVER_WINDOW=65535, MTU=1400, MIN_UPLOAD_MBPS=10, MIN_POST_MBPS=20)),
    ("fast-slow-fast-upload", dict(SIZE_MB=64, RATE=62500000, UP_PROFILE="capacity-drop", DELAY_MS=10, QUEUE_BYTES=4194304, SERVER_WINDOW=65535, MTU=1400, MODES="UDU", MIN_UPLOAD_MBPS=10, MIN_FINAL_UPLOAD_MBPS=40)),
    ("asymmetric-1up-100down", dict(SIZE_KB=512, UP_RATE=125000, DOWN_RATE=12500000, QUEUE_BYTES=262144, SERVER_WINDOW=65535)),
    ("asymmetric-100up-1down", dict(SIZE_KB=512, UP_RATE=12500000, DOWN_RATE=125000, QUEUE_BYTES=262144, SERVER_WINDOW=65535)),
    ("high-rtt", dict(SIZE_KB=128, DELAY_MS=500, QUEUE_BYTES=1048576)),
    ("cellular-uplink", dict(SIZE_MB=2, UP_PROFILE="cellular", DOWN_PROFILE="jitter", DELAY_MS=50)),
    ("cellular-both", dict(SIZE_MB=2, PROFILE="cellular", DELAY_MS=50)),
    ("tcp-hol-stalls", dict(SIZE_KB=512, PROFILE="tcp-loss", DELAY_MS=50)),
    ("ack-compression", dict(SIZE_MB=1, DOWN_PROFILE="ack-compression", DELAY_MS=50)),
    ("upload-blackout", dict(SIZE_MB=2, UP_PROFILE="outage", DELAY_MS=100)),
    ("long-upload-blackout", dict(SIZE_MB=2, UP_PROFILE="long-outage", DELAY_MS=100)),
    ("long-download-blackout", dict(SIZE_MB=2, DOWN_PROFILE="long-outage", DELAY_MS=100)),
    ("download-blackout", dict(SIZE_MB=2, DOWN_PROFILE="outage", DELAY_MS=100)),
    ("background-contention", dict(SIZE_MB=2, UP_PROFILE="contention", DELAY_MS=50)),
    ("mixed-small-queue", dict(SIZE_MB=1, CONCURRENT=1, MODES="DU" + "T" * 12, DELAY_MS=50, QUEUE_BYTES=32768, INTERACTIVE_DELAY_MS=300, MAX_TINY_P95_MS=4000)),
    ("mixed-deep-queue", dict(SIZE_MB=1, CONCURRENT=1, MODES="DU" + "T" * 12, DELAY_MS=50, QUEUE_BYTES=1048576, INTERACTIVE_DELAY_MS=300, MAX_TINY_P95_MS=4000)),
    ("slow-reader", dict(SIZE_KB=256, CONCURRENT=1, MODES="DDDTTTTTT", RECEIVER_DELAY_MS=10, INTERACTIVE_DELAY_MS=300, DELAY_MS=50, MAX_TINY_P95_MS=4000)),
    ("idle-restart", dict(SIZE_KB=256, IDLE_MS=3000, DELAY_MS=50)),
    ("churn-80", dict(SIZE_KB=32, MODES="T" * 80, DELAY_MS=25)),
    ("tls-during-bulk", dict(TLS=1, SIZE_KB=256, CONCURRENT=1, MODES="DU" + "T" * 8, INTERACTIVE_DELAY_MS=300, DELAY_MS=50, MAX_TINY_P95_MS=4000)),
    ("record-loss-bursts", dict(SIZE_KB=512, LOSS_EVERY=71, LOSS_BURST=3, DUP_EVERY=17, DELAY_MS=50)),
]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=("race", "performance", "both"), default="both")
    parser.add_argument("--case", action="append", help="Select a case by name; repeat to select several")
    parser.add_argument("--raw-only", action="store_true", help="Run only the packetization-specific bare-carrier reference")
    parser.add_argument("--protocol-only", action="store_true", help="Skip raw-carrier calibration; run protocol transfers and carrier correctness checks")
    parser.add_argument("--profile", action="store_true", help="Capture Go CPU, heap, blocking and mutex profiles of the production-stack test only")
    parser.add_argument("--size-mb", type=int, help="Override bulk transfer size; use longer transfers to measure steady state")
    parser.add_argument("--repeat", type=int, default=1)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--soak-seconds", type=int, default=0, help="Add sustained cellular-profile load for this duration")
    args = parser.parse_args()
    if args.protocol_only and args.raw_only:
        parser.error("--protocol-only and --raw-only are mutually exclusive")
    if args.profile and (args.raw_only or args.mode != "performance"):
        parser.error("profiling requires --mode performance and excludes --raw-only")
    if args.size_mb is not None and args.size_mb < 1:
        parser.error("--size-mb must be positive")
    if args.soak_seconds < 0:
        parser.error("--soak-seconds must be nonnegative")
    if args.repeat < 1:
        parser.error("--repeat must be positive")
    cases = [(name, cfg) for name, cfg in CASES if not args.case or name in args.case]
    if not cases or (args.case and set(args.case) - {name for name, _ in CASES}):
        parser.error("unknown case")
    if args.soak_seconds:
        cases.append(("cellular-soak", dict(SIZE_KB=256, SOAK_SECONDS=args.soak_seconds, MODES="DUT", UP_PROFILE="cellular", DOWN_PROFILE="jitter", DELAY_MS=50)))
    if args.size_mb is not None:
        cases = [(name, {**{k: v for k, v in cfg.items() if k != "SIZE_KB"}, "SIZE_MB": args.size_mb}) for name, cfg in cases]
    go = shutil.which(os.environ.get("GO_BIN") or "go")
    if not go:
        parser.error("Go toolchain missing; set GO_BIN")
    # Builds run in the generated module directory; preserve relative selections.
    go = str(Path(go).resolve())
    output = (args.output or ROOT / "build" / "uplink-tests" / time.strftime("%Y%m%d-%H%M%S")).resolve()
    output.mkdir(parents=True, exist_ok=True)
    work = output / "module"
    subprocess.run([sys.executable, str(HERE / "prepare.py"), str(work)], check=True)
    env = os.environ.copy()
    env.setdefault("GOPROXY", "off")
    env.setdefault("GOCACHE", str(output / "go-cache"))
    # Record the actual environment; do not assume race-instrumented results are line-rate evidence.
    metadata = dict(kernel=platform.platform(), python=sys.version, go=subprocess.check_output([go, "version"], text=True).strip(), cpu_count=os.cpu_count(), mode=args.mode, raw_only=args.raw_only, protocol_only=args.protocol_only, profile=args.profile, raw_down_chunk_bytes=16000, raw_up_chunk_default_bytes=500)
    (output / "environment.json").write_text(json.dumps(metadata, indent=2) + "\n")
    results = []
    modes = ("performance", "race") if args.mode == "both" else (args.mode,)
    pprof = None
    if args.profile:
        configured = os.environ.get("PPROF_BIN")
        if configured:
            pprof = [configured]
        else:
            probe = subprocess.run([go, "tool", "-n", "pprof"], env=env, capture_output=True, text=True)
            if probe.returncode == 0:
                pprof = [go, "tool", "pprof"]
            else:
                # Some sandbox toolchains include source but omit the binary.
                tool = output / "pprof"
                build = subprocess.run([go, "build", "-o", str(tool), "cmd/pprof"], env=env, capture_output=True, text=True)
                if build.returncode == 0:
                    pprof = [str(tool)]
                else:
                    print("pprof report tool unavailable; retaining raw profiles for analysis on another host", file=sys.stderr)
    failed = False
    for mode in modes:
        binary = output / ("uplink-" + mode + ".test")
        build = [go, "test", "-c", "-o", str(binary)]
        if mode == "race":
            build.append("-race")
        subprocess.run(build + ["."], cwd=work, env=env, check=True)
        for name, cfg in cases:
            for repetition in range(1, args.repeat + 1):
                label = f"{mode}-{name}-{repetition}"
                runenv = env.copy()
                for key in {key for _, config in CASES for key in config} | {"SIZE_MB", "SIZE_KB", "UP_RATE", "DOWN_RATE", "PROFILE", "UP_PROFILE", "DOWN_PROFILE", "LOSS_EVERY", "DUP_EVERY", "SOAK_SECONDS"}:
                    runenv.pop(key, None)
                runenv.update(BBR="1", FAIR="1", SERVER_FAIR="1", BBR_RATES_FILE=str(output / (label + ".rates")))
                runenv.update({key: str(value) for key, value in cfg.items()})
                started = time.monotonic()
                with (output / (label + ".log")).open("w") as log:
                    try:
                        limit = max(120, args.soak_seconds + 90)
                        selection = ["-test.run=^TestRawCarrier$"] if args.raw_only else []
                        if args.protocol_only:
                            selection = ["-test.run=^(TestControlledTransfer|TestCarrier.*)$"]
                        if args.profile:
                            selection = ["-test.run=^TestControlledTransfer$",
                                         "-test.cpuprofile=" + str(output / (label + ".cpu.pprof")),
                                         "-test.memprofile=" + str(output / (label + ".heap.pprof")),
                                         "-test.blockprofile=" + str(output / (label + ".block.pprof")),
                                         "-test.blockprofilerate=1000000",
                                         "-test.mutexprofile=" + str(output / (label + ".mutex.pprof")),
                                         "-test.mutexprofilefraction=10"]
                        proc = subprocess.run([str(binary), "-test.v", f"-test.timeout={limit}s"] + selection, cwd=work, env=runenv, stdout=log, stderr=subprocess.STDOUT, timeout=limit+10)
                        code = proc.returncode
                    except subprocess.TimeoutExpired:
                        code = 124
                content = (output / (label + ".log")).read_text()
                if args.profile and pprof:
                    for kind, flags in (("cpu", []), ("heap", ["-sample_index=alloc_space"]), ("block", ["-cum"]), ("mutex", ["-cum"])):
                        profile = output / (label + "." + kind + ".pprof")
                        if profile.exists():
                            report = subprocess.run(pprof + ["-top", "-nodecount=40"] + flags + [str(binary), str(profile)], env=env, capture_output=True, text=True)
                            (output / (label + "." + kind + ".txt")).write_text(report.stdout + report.stderr)
                            if report.returncode:
                                print(f"pprof report failed for {profile}; raw profile retained", file=sys.stderr)
                transfers = re.findall(r"(?<!raw )mode=(\w) bytes=(\d+) seconds=([\d.]+) Mbps=([\d.]+).*?err=(.*)", content)
                raw = re.findall(r"raw mode=(\w) bytes=(\d+) seconds=([\d.]+) Mbps=([\d.]+)", content)
                summaries = re.findall(r"summary (.*)", content)
                summary = {key: float(value) for key, value in re.findall(r"(\w+)=([\d.]+)", summaries[-1])} if summaries else {}
                if not any(cfg.get(key) for key in ("PROFILE", "UP_PROFILE", "DOWN_PROFILE")) and len(transfers)==3 and len(raw)==3:
                    summary["final_down_raw_fraction"] = float(transfers[-1][3]) / float(raw[-1][3]) if float(raw[-1][3]) else 0
                    summary["up_raw_fraction"] = float(transfers[1][3]) / float(raw[1][3]) if float(raw[1][3]) else 0
                result = dict(name=name, mode=mode, repetition=repetition, exit=code, seconds=round(time.monotonic()-started, 3), settings=cfg, raw_only=args.raw_only, transfers=transfers, raw=raw, summary=summary)
                results.append(result)
                failed |= code != 0
                (output / "results.json").write_text(json.dumps(results, indent=2) + "\n")
                print(json.dumps(result), flush=True)
    print(f"Results: {output}; passed {sum(r['exit']==0 for r in results)}/{len(results)}", flush=True)
    return int(failed)


if __name__ == "__main__":
    raise SystemExit(main())
