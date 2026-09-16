#!/usr/bin/env python3
"""独立したCompose環境で負荷・観測・DB整合性照合を同じ条件で再実行する。"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
DOCKER = shutil.which("docker") or "/Applications/Docker.app/Contents/Resources/bin/docker"
COMPOSE = [DOCKER, "compose", "-p", "ec-learning-load", "-f", str(ROOT / "compose.load.yaml")]


def run(args, env, **kwargs):
    return subprocess.run(args, cwd=ROOT, env=env, check=True, text=True, **kwargs)


def sql(query, env):
    return run(COMPOSE + ["exec", "-T", "db", "psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-U", "ec", "-d", "ec_load", "-c", query], env, capture_output=True).stdout.strip()


def metrics(port):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/metrics", timeout=3) as response:
        return json.load(response)


def validate(env, summary, item_count):
    # DBの販売数量と残在庫、保存した全注文・明細、およびクライアントの結果を照合する。
    result = json.loads(sql(f"""
      SELECT json_build_object(
        'orders', (SELECT count(*) FROM orders),
        'success', (SELECT count(*) FROM orders WHERE status='shipping_requested'),
        'shortage', (SELECT count(*) FROM orders WHERE status='failed' AND failure_reason='out_of_stock'),
        'items', (SELECT count(*) FROM order_items),
        'invalid_products', (SELECT count(*) FROM products p LEFT JOIN
          (SELECT i.product_id,sum(i.quantity) AS sold FROM order_items i JOIN orders o ON o.id=i.order_id
           WHERE o.status='shipping_requested' GROUP BY i.product_id) s ON s.product_id=p.id
          WHERE p.stock < 0 OR p.stock + COALESCE(s.sold,0) <> 1000000),
        'invalid_orders', (SELECT count(*) FROM orders o WHERE
          (SELECT count(*) FROM order_items i WHERE i.order_id=o.id) <> {item_count}
          OR (o.status='shipping_requested' AND (o.failure_reason IS NOT NULL OR o.payment_status<>'succeeded' OR o.shipping_status<>'requested'))),
        'invalid_items', (SELECT count(*) FROM order_items i JOIN products p ON p.id=i.product_id
          WHERE i.quantity<>1 OR i.price_yen<>500 OR i.product_name<>p.name)
      )""", env))
    values = summary["metrics"]
    expected_success = int(values["successful_orders"]["values"]["count"])
    expected_shortage = int(values["shortage_orders"]["values"]["count"])
    result["client_checks_passed"] = values.get("checks", {}).get("values", {}).get("rate") == 1
    result["passed"] = (
        result["client_checks_passed"] and result["orders"] == expected_success + expected_shortage
        and result["success"] == expected_success and result["shortage"] == expected_shortage
        and result["items"] == result["orders"] * item_count
        and result["invalid_products"] == result["invalid_orders"] == result["invalid_items"] == 0
    )
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--variants", nargs="+", choices=["baseline", "current"], default=["baseline", "current"])
    p.add_argument("--vus", nargs="+", type=int, default=[1, 5, 10, 20])
    p.add_argument("--seconds", type=int, default=30)
    p.add_argument("--items", type=int, default=20)
    p.add_argument("--workload", choices=["shared", "separate"], default="shared")
    p.add_argument("--port", type=int, default=18080)
    args = p.parse_args()
    if not 1 <= args.items <= 100 or not 1 <= args.seconds <= 600 or any(not 1 <= v <= 100 for v in args.vus):
        p.error("items/vus must be 1..100; seconds must be 1..600")
    # 同じ固定Composeプロジェクトへ2つの実験から初期化をかけない。
    lock = open(Path(tempfile.gettempdir()) / "ec-learning-load.lock", "w")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        p.error("another load/run.py is running")
    out = ROOT / "output" / "load" / time.strftime("%Y%m%d-%H%M%S")
    out.mkdir(parents=True, exist_ok=False)
    env = {**os.environ, "LOAD_PORT": str(args.port), "LOAD_RESULTS_DIR": str(out)}
    manifest = {"conditions": vars(args), "thresholds": {"p95_ms": 500, "system_error_rate_less_than": 0.01},
                "docker": json.loads(run([DOCKER, "info", "--format", '{{json .}}'], env, capture_output=True).stdout),
                "git_head": run(["git", "rev-parse", "HEAD"], env, capture_output=True).stdout.strip()}
    # Docker infoの全体を成果物へ複製せず、実験に必要な資源情報だけ記録する。
    manifest["docker"] = {key: manifest["docker"].get(key) for key in ["NCPU", "MemTotal", "ServerVersion", "Architecture", "KernelVersion"]}
    manifest["source_sha256"] = {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest()
                                 for path in [*sorted((ROOT / "backend").glob("*.go")), ROOT / "load/orders.js", ROOT / "load/run.py", ROOT / "compose.load.yaml"]}
    (out / "manifest.json").write_text(json.dumps(manifest, indent=2))
    print(f"Results: {out}", flush=True)
    all_results = []
    try:
        run(COMPOSE + ["build", "api"], env)
        for variant in args.variants:
            for vus in args.vus:
                stage = out / f"{variant}-vus{vus}"
                stage.mkdir()
                stage_env = {**env, "ORDER_VARIANT": variant, "VUS": str(vus), "DURATION": f"{args.seconds}s",
                             "ITEMS": str(args.items), "WORKLOAD": args.workload, "LOAD_RESULTS_DIR": str(stage)}
                # WAL・キャッシュ・tmpfs使用量を前条件から持ち越さない。
                run(COMPOSE + ["stop", "api", "db"], stage_env, capture_output=True)
                run(COMPOSE + ["rm", "-f", "db"], stage_env, capture_output=True)
                run(COMPOSE + ["up", "-d", "--wait", "db"], stage_env, capture_output=True)
                run(COMPOSE + ["exec", "-T", "db", "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "ec", "-d", "ec_load", "-f", "/experiment/seed.sql"], stage_env, capture_output=True)
                run(COMPOSE + ["up", "-d", "--force-recreate", "api"], stage_env, capture_output=True)
                for _ in range(100):
                    try:
                        initial = metrics(args.port)
                        if initial["variant"] != variant:
                            raise RuntimeError("wrong load API variant")
                        break
                    except (OSError, ValueError):
                        time.sleep(0.1)
                else:
                    raise RuntimeError("load API did not become ready")
                print(f"Running {variant}: {vus} VUs x {args.seconds}s, {args.items} items, {args.workload}", flush=True)
                with (stage / "k6.log").open("w") as log:
                    proc = subprocess.Popen(COMPOSE + ["run", "--rm", "--no-deps", "k6"], cwd=ROOT, env=stage_env, stdout=log, stderr=subprocess.STDOUT, text=True)
                    samples = []
                    try:
                        while proc.poll() is None:
                            snapshot = {"time": time.time(), "api": metrics(args.port)}
                            # compose psにone-offのk6が含まれない版もあるため、プロジェクトラベルで列挙する。
                            ids = run([DOCKER, "ps", "-q", "--filter", "label=com.docker.compose.project=ec-learning-load"], stage_env, capture_output=True).stdout.split()
                            if ids:
                                stats = subprocess.run([DOCKER, "stats", "--no-stream", "--format", "{{json .}}", *ids], cwd=ROOT, env=stage_env, capture_output=True, text=True)
                                snapshot["containers"] = [json.loads(line) for line in stats.stdout.splitlines() if line.strip()]
                                if stats.returncode:
                                    # 一覧取得とstatsの間に終了・削除されたk6は採取できない。
                                    # 注文結果は保持し、採取失敗も成果物に明示する。
                                    snapshot["container_sample_error"] = stats.stderr.strip()
                            snapshot["db"] = json.loads(sql("""SELECT COALESCE(json_agg(x),'[]'::json) FROM
                              (SELECT pid,state,wait_event_type,wait_event,pg_blocking_pids(pid) AS blocking_pids,
                               EXTRACT(EPOCH FROM clock_timestamp()-xact_start)*1000 AS transaction_age_ms
                               FROM pg_stat_activity WHERE application_name='learning-load-api') x""", stage_env))
                            samples.append(snapshot)
                            time.sleep(0.2)
                    finally:
                        if proc.poll() is None:
                            proc.terminate()
                            proc.wait(timeout=15)
                    code = proc.returncode
                (stage / "samples.json").write_text(json.dumps(samples, indent=2))
                if code not in (0, 99):
                    raise RuntimeError(f"k6 failed with exit {code}; see {stage / 'k6.log'}")
                summary = json.loads((stage / "k6.json").read_text())
                sqlstats = json.loads(sql("""SELECT COALESCE(json_agg(x),'[]'::json) FROM
                  (SELECT query,calls,rows,total_exec_time,mean_exec_time,shared_blks_hit,shared_blks_read
                   FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
                     AND query NOT ILIKE '%pg_stat%' ORDER BY total_exec_time DESC LIMIT 40) x""", stage_env))
                (stage / "sql.json").write_text(json.dumps(sqlstats, indent=2))
                integrity = validate(stage_env, summary, args.items)
                (stage / "integrity.json").write_text(json.dumps(integrity, indent=2))
                ids = ','.join(str(i) for i in range(1, args.items+1))
                query = ("SELECT id,name,price_yen,stock FROM products WHERE id=1 FOR UPDATE" if variant == "baseline"
                         else f"SELECT id,name,price_yen,stock FROM products WHERE id=ANY(ARRAY[{ids}]::bigint[]) ORDER BY id FOR UPDATE")
                (stage / "explain.txt").write_text(sql("EXPLAIN (ANALYZE,BUFFERS) " + query, stage_env) + "\n")
                (stage / "api.log").write_text(run(COMPOSE + ["logs", "--no-color", "api"], stage_env, capture_output=True).stdout)
                time.sleep(1)
                (stage / "after.json").write_text(json.dumps(metrics(args.port), indent=2))
                v = summary["metrics"]
                result = {"variant": variant, "vus": vus, "requests": v["http_reqs"]["values"]["count"],
                          "rps": v["http_reqs"]["values"]["rate"], "p95_ms": v["http_req_duration"]["values"]["p(95)"],
                          "p99_ms": v["http_req_duration"]["values"]["p(99)"], "system_error_rate": v["system_errors"]["values"]["rate"],
                          "shortage_rate": v["stock_shortage"]["values"]["rate"],
                          "acquire_avg_ms": v["pool_acquire_ms"]["values"]["avg"],
                          "sql_avg_ms": v["sql_ms"]["values"]["avg"], "lock_sql_avg_ms": v["lock_sql_ms"]["values"]["avg"],
                          "sql_calls_avg": v["sql_calls"]["values"]["avg"], "k6_exit": code, "integrity": integrity["passed"]}
                result["container_sample_errors"] = sum("container_sample_error" in s for s in samples)
                all_results.append(result)
                (out / "comparison.json").write_text(json.dumps(all_results, indent=2))
                print(json.dumps(result), flush=True)
                if not integrity["passed"]:
                    raise RuntimeError("DB integrity/client result reconciliation failed")
    finally:
        # このファイル専用のCompose環境だけを停止・削除。通常のec-learningには触れない。
        run(COMPOSE + ["down"], env)
        lock.close()
    print(f"Comparison: {out / 'comparison.json'}", flush=True)


if __name__ == "__main__":
    main()
