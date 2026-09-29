"""専用Compose環境で正常・DB待ち・タイムアウト・手動再開を作り、Jaegerとログを照合する。"""
import argparse
import json
import os
from pathlib import Path
import select
import subprocess
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--project', required=True, help='起動済みの検証専用Composeプロジェクト名')
parser.add_argument('--base-url', default='http://127.0.0.1:3002')
parser.add_argument('--jaeger-url', default='http://127.0.0.1:16687')
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
compose = ['docker', 'compose', '-p', args.project]


def call(path, body=None, expected=200):
    req = Request(args.base_url + path, data=None if body is None else json.dumps(body).encode(),
                  headers={'Content-Type': 'application/json', 'X-Cart-Request': '1'})
    start = time.monotonic()
    try:
        response = urlopen(req, timeout=8)
    except HTTPError as error:
        response = error
    with response:
        payload = json.load(response)
        assert response.status == expected, (response.status, payload)
        return payload, response.headers.get('X-Trace-ID'), time.monotonic() - start


def locked_order(product, seconds, expected):
    # 通常APIに遅延パラメーターを設けず、検証用DBでだけロックする。
    proc = subprocess.Popen(compose + ['exec', '-T', 'payment-db', 'psql', '-U', 'payment', '-d', 'payment',
                                       '-qAt', '-v', 'ON_ERROR_STOP=1'], cwd=root,
                            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, bufsize=1)
    timer = None
    release_errors = []
    try:
        proc.stdin.write("SET lock_timeout='5s'; SET idle_in_transaction_session_timeout='5s';\n"
                         "BEGIN; LOCK TABLE payments IN ACCESS EXCLUSIVE MODE; SELECT 'READY';\n")
        proc.stdin.flush()
        if not select.select([proc.stdout], [], [], 10)[0] or proc.stdout.readline().strip() != 'READY':
            raise RuntimeError('DBロック取得を確認できませんでした')
        def release():
            try:
                proc.stdin.write('COMMIT;\n'); proc.stdin.flush()
            except (BrokenPipeError, ValueError) as error:
                release_errors.append(str(error))
        timer = threading.Timer(seconds, release)
        timer.start()
        return call('/api/orders', {'productId': product, 'quantity': 1}, expected)
    finally:
        if timer:
            timer.join(timeout=5)
        if proc.stdin:
            proc.stdin.close()
        try:
            proc.wait(timeout=8)
        except subprocess.TimeoutExpired:
            proc.terminate(); proc.wait(timeout=5)
        error = proc.stderr.read()
        if proc.returncode or release_errors:
            raise RuntimeError(f'ロック解除に失敗: {error} {release_errors}')


def trace_data(trace_id, required):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        try:
            with urlopen(args.jaeger_url + '/api/traces/' + trace_id, timeout=3) as response:
                data = json.load(response)['data'][0]
            names = {span['operationName'] for span in data['spans']}
            services = {p['serviceName'] for p in data['processes'].values()}
            if required <= names and {'order', 'payment'} <= services:
                return data
        except (HTTPError, URLError, IndexError, KeyError):
            pass
        time.sleep(.25)
    raise AssertionError('Jaegerに必要なspanが揃いません: ' + str(trace_id))


def show(label, result, required, is_error=False):
    payload, trace_id, elapsed = result
    assert trace_id and len(trace_id) == 32, payload
    order_id = payload.get('orderId') or payload['order']['id']
    data = trace_data(trace_id, required)
    spans = data['spans']
    tags = lambda span: {t['key']: t['value'] for t in span.get('tags', [])}
    assert any(tags(s).get('order_id') == order_id for s in spans)
    assert any(tags(s).get('error') is True for s in spans) == is_error
    # サービス名が同じtrace内にあるだけでなく、Payment serverの親がOrder clientかを確認。
    by_id = {s['spanID']: s for s in spans}
    linked = False
    for span in spans:
        if data['processes'][span['processID']]['serviceName'] != 'payment':
            continue
        for ref in span.get('references', []):
            parent = by_id.get(ref['spanID'])
            if parent and data['processes'][parent['processID']]['serviceName'] == 'order':
                linked = True
    assert linked, 'サービス間の親子関係がありません'
    print(f'\n{label}: {elapsed:.3f}s\n  order_id={order_id}\n  trace_id={trace_id}\n  {args.jaeger_url}/trace/{trace_id}', flush=True)
    for span in sorted(spans, key=lambda s: s['startTime']):
        service = data['processes'][span['processID']]['serviceName']
        print(f"  {service:7} {span['operationName']:38} {span['duration']/1000:8.1f}ms", flush=True)
    return order_id, trace_id


products = call('/api/products')[0]['products']
product = next(p['id'] for p in products if p['stock'] >= 4)
normal = call('/api/orders', {'productId': product, 'quantity': 1}, 201)
normal_id, normal_trace = show('1 正常注文', normal, {'order.reserve', 'order.finalize', 'payment.store.create'})
slow = locked_order(product, .6, 201)
slow_id, slow_trace = show('2 PaymentのDB待ち', slow, {'order.finalize', 'payment.store.get'})
assert slow[2] >= .35, '遅延を観測できませんでした'
unknown = locked_order(product, 1.6, 503)
unknown_id, unknown_trace = show('3 タイムアウト', unknown, {'order.resume', 'payment.store.get'}, True)
assert call('/api/orders/' + unknown_id)[0]['order']['status'] == 'processing'
recovered = call('/api/orders/' + unknown_id + '/resume', {})
recovered_id, recovered_trace = show('4 同じ注文の手動再開', recovered, {'order.finalize', 'payment.store.create'})
assert unknown_id == recovered_id and unknown_trace != recovered_trace
assert recovered[0]['order']['status'] == 'shipping_requested'
cancelled = call('/api/orders', {'productId': product, 'quantity': 1, 'shippingMode': 'fail'}, 201)
show('5 配送失敗の補償', cancelled, {'payment.store.cancel', 'order.finalize'})
assert cancelled[0]['order']['paymentStatus'] == 'cancelled'

raw = subprocess.check_output(compose + ['logs', '--no-color', '--no-log-prefix', 'api', 'payment'], cwd=root, text=True)
logs = []
for line in raw.splitlines():
    try:
        logs.append(json.loads(line))
    except json.JSONDecodeError:
        pass
for trace_id in [normal_trace, slow_trace, unknown_trace, recovered_trace]:
    related = [r for r in logs if r.get('trace_id') == trace_id]
    assert {'order', 'payment'} <= {r.get('service') for r in related}
    assert all(len(r.get('span_id', '')) == 16 for r in related)
print('\nPASS: サービス間の親子関係、失敗span、別traceでの再開、JSONログのIDを照合しました。', flush=True)
