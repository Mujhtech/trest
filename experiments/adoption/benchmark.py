#!/usr/bin/env python3
"""Build once, rotate framework order, and retain reproducible benchmark evidence."""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import statistics
import subprocess
import tempfile


FRAMEWORKS = ["net_http", "trest_app", "trest", "huma", "chi", "gin", "echo"]
WORKLOADS = ["BenchmarkHelloJSON", "BenchmarkDuplicateInvoice"]
RESULT = re.compile(
    r"^(Benchmark\w+)/framework=(\w+)(?:-\d+)?\s+\d+\s+"
    r"([\d.]+) ns/op\s+(\d+) B/op\s+(\d+) allocs/op$", re.MULTILINE
)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path, help="new directory for raw samples and metadata")
    parser.add_argument("--rounds", type=int, default=14, help="samples per case (default: 14)")
    parser.add_argument("--benchtime", default="500ms", help="Go benchmark duration per sample")
    parser.add_argument("--gomaxprocs", type=int, default=1, help="fixed GOMAXPROCS (default: 1)")
    args = parser.parse_args()
    if args.rounds < 1 or args.gomaxprocs < 1:
        parser.error("rounds and gomaxprocs must be positive")
    if not re.fullmatch(r"[1-9][0-9]*(?:ms|s|x)", args.benchtime):
        parser.error("benchtime must be a positive integer with ms, s, or x suffix")
    benchstat = shutil.which("benchstat")
    if benchstat is None:
        parser.error("benchstat must be installed and available on PATH")
    args.output = args.output.resolve()
    args.output.mkdir(parents=True, exist_ok=False)
    module = Path(__file__).resolve().parent
    root = module.parent.parent
    env = dict(os.environ, GOWORK="off", GIN_MODE="release", GOMAXPROCS=str(args.gomaxprocs))

    def command(argv, cwd=module):
        return subprocess.check_output(argv, cwd=cwd, env=env, text=True)

    def digest(path):
        return hashlib.sha256(path.read_bytes()).hexdigest()

    source_hashes = {}
    for path in sorted(root.rglob("*")):
        relative = path.relative_to(root)
        if path.is_file() and ".git" not in relative.parts and relative.parts[0] != "docs":
            if path.suffix in {".go", ".mod", ".sum"} or path == Path(__file__).resolve():
                source_hashes[str(relative)] = digest(path)

    metadata = {
        "started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "go_version": command(["go", "version"]).strip(),
        "platform": platform.platform(),
        "git_head": command(["git", "rev-parse", "HEAD"], root).strip(),
        "git_status": command(["git", "status", "--short"], root).splitlines(),
        "gomaxprocs": args.gomaxprocs,
        "gin_mode": "release",
        "benchtime": args.benchtime,
        "rounds": args.rounds,
        "environment": {key: env.get(key, "") for key in
                        ["GOFLAGS", "GOTOOLCHAIN", "GOEXPERIMENT", "GOGC", "GOMEMLIMIT", "GODEBUG"]},
        "compiled_modules": sorted(set(filter(None, command([
            "go", "list", "-deps", "-test", "-f",
            "{{if .Module}}{{.Module.Path}}@{{.Module.Version}}{{if .Module.Replace}} => {{.Module.Replace.Path}}{{end}}{{end}}",
            "."]).splitlines()))),
        "source_sha256": source_hashes,
        "benchstat_build": command(["go", "version", "-m", benchstat]).splitlines(),
        "execution_order": [],
    }
    metadata_path = args.output / "run.json"

    def save_metadata():
        metadata_path.write_text(json.dumps(metadata, indent=2) + "\n")

    save_metadata()
    with tempfile.TemporaryDirectory(prefix="trest-framework-bench-") as temp:
        binary = Path(temp) / "adoption.test"
        subprocess.run(["go", "test", "-c", "-o", str(binary), "."], cwd=module, env=env, check=True)
        metadata["binary_sha256"] = digest(binary)
        save_metadata()
        raw = args.output / "samples.txt"
        with raw.open("w") as output:
            for index in range(args.rounds):
                shift = index % len(FRAMEWORKS)
                order = FRAMEWORKS[shift:] + FRAMEWORKS[:shift]
                if (index // len(FRAMEWORKS)) % 2:
                    order = list(reversed(order))
                workloads = WORKLOADS if index % 2 == 0 else list(reversed(WORKLOADS))
                for framework in order:
                    for workload in workloads:
                        selector = f"^{workload}$/^framework={framework}$"
                        argv = [str(binary), "-test.run=^$", f"-test.bench={selector}",
                                f"-test.benchtime={args.benchtime}", "-test.benchmem", "-test.count=1"]
                        result = command(argv)
                        matches = list(RESULT.finditer(result))
                        if len(matches) != 1 or matches[0].groups()[:2] != (workload, framework):
                            raise RuntimeError(f"Unexpected benchmark output for {selector}: {result}")
                        output.write(result)
                        output.flush()
                        metadata["execution_order"].append({"round": index + 1, "framework": framework,
                                                            "workload": workload})
                save_metadata()
                print(f"Completed round {index + 1}/{args.rounds}", flush=True)

    groups = {}
    for match in RESULT.finditer(raw.read_text()):
        workload, framework, ns, size, allocs = match.groups()
        groups.setdefault(workload + "/" + framework, []).append(
            {"ns_per_op": float(ns), "bytes_per_op": int(size), "allocs_per_op": int(allocs)})
    summary = {}
    for key, values in groups.items():
        if len(values) != args.rounds:
            raise RuntimeError(f"Missing samples for {key}")
        summary[key] = {"samples": len(values), **{
            metric: statistics.median(value[metric] for value in values) for metric in values[0]
        }}
    (args.output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    (args.output / "benchstat.txt").write_text(command([benchstat, str(raw)]))
    (args.output / "comparison.txt").write_text(command([
        benchstat, "-row", ".name", "-col",
        "/framework@(net_http chi gin echo huma trest trest_app)", str(raw)]))
    metadata["completed_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    metadata["samples_sha256"] = digest(raw)
    save_metadata()
    print(f"Saved raw samples, medians, benchstat, and provenance to {args.output}")


if __name__ == "__main__":
    main()
