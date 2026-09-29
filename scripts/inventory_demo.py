"""Inventory分離のデモ。専用環境で実行する。--lifecycle はサービスを停止・復旧する。"""
import argparse
import json
import os
import subprocess
import time
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import Request, urlopen

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--lifecycle', action='store_true')
args = parser.parse_args()
base = os.environ.get('BASE_URL', 'http://127.0.0.1:3000')
inv = os.environ.get('INVENTORY_BASE_URL', 'http://127.0.0.1:8086')
jaeger = os.environ.get('JAEGER_URL', 'http://127.0.0.1:16686')
root = Path(__file__).resolve().parents[1]


def call(origin, path, body=None, expected=200):
    req = Request(origin + path, data=None if body is None else json.dumps(body).encode(),
                  headers={'Content-Type': 'application/json', 'X-Cart-Request': '1'})
    try:
        response = urlopen(req, timeout=8)
    except HTTPError as error:
        response = error
    with response:
        data = json.load(response)
        assert response.status == expected, (path, response.status, data)
        return data, response.headers.get('X-Trace-ID')


def compose(*args):
    subprocess.run(['docker', 'compose', *args], cwd=root, check=True)


def available(pid):
    return call(inv, '/stocks?ids=' + str(pid))[0]['stocks'][0]['available']


def trace(tid):
    for _ in range(30):
        try:
            data = call(jaeger, '/api/traces/' + tid)[0]['data']
        except (AssertionError, KeyError):
            data = []
        if data:
            t = data[0]
            services = {p['serviceName'] for p in t['processes'].values()}
            spans = {s['spanID']: s for s in t['spans']}
            linked = set()
            for s in spans.values():
                if any(r['spanID'] in spans and t['processes'][spans[r['spanID']]['processID']]['serviceName'] == 'order'
                       for r in s.get('references', [])):
                    linked.add(t['processes'][s['processID']]['serviceName'])
            if {'order', 'inventory', 'payment', 'shipping'} <= services and {'inventory', 'payment', 'shipping'} <= linked:
                print('PASS four services and parent links:', jaeger + '/trace/' + tid, flush=True)
                return
        time.sleep(.5)
    raise AssertionError('four-service trace not found: ' + tid)


products = call(base, '/api/products')[0]['products']
p = next(p for p in products if p['stockKnown'] and p['stock'] >= 4)
pid = p['id']
for pay, ship, status in [('success', 'success', 'committed'), ('fail', 'success', 'released'), ('success', 'fail', 'released')]:
    before = available(pid)
    order, tid = call(base, '/api/orders', {'productId': pid, 'quantity': 1, 'paymentMode': pay, 'shippingMode': ship}, 201)
    order = order['order']
    reservation = call(inv, '/reservations/' + order['id'])[0]['reservation']
    assert order['inventoryStatus'] == reservation['status'] == status
    assert available(pid) == before - (1 if status == 'committed' else 0)
    replay = call(inv, '/reservations', {'orderId': order['id'], 'items': reservation['items']}, 201)[0]['reservation']
    assert replay == reservation
    assert call(base, '/api/orders/' + order['id'] + '/resume', {})[0]['order'] == order
    print('PASS', pay, ship, status, 'order_id=' + order['id'], flush=True)
    if pay == ship == 'success':
        trace(tid)

before = available(pid)
order = call(base, '/api/orders', {'productId': pid, 'quantity': before + 1}, 201)[0]['order']
assert (order['failureReason'], order['paymentStatus'], order['inventoryStatus']) == ('out_of_stock', 'not_started', 'rejected')
assert available(pid) == before
print('PASS insufficient stock: payment not started', flush=True)

if args.lifecycle:
    pending = None
    before = available(pid)
    try:
        compose('stop', 'inventory')
        products = call(base, '/api/products')[0]['products']
        assert all(not p['stockKnown'] for p in products)
        data, _ = call(base, '/api/orders', {'productId': pid, 'quantity': 1}, 503)
        pending = data['orderId']
        order = call(base, '/api/orders/' + pending)[0]['order']
        assert (order['status'], order['paymentStatus'], order['inventoryStatus']) == ('processing', 'not_started', 'pending')
        compose('restart', 'api')
        print('PASS stopped Inventory: unknown stock; pending order persists across Order restart', pending, flush=True)
    finally:
        compose('up', '-d', '--wait', '--wait-timeout', '120', 'inventory')
    if pending:
        order, tid = call(base, '/api/orders/' + pending + '/resume', {})
        assert order['order']['inventoryStatus'] == 'committed'
        assert available(pid) == before - 1
        trace(tid)
        saved = call(inv, '/reservations/' + pending)[0]
        compose('restart', 'inventory-db', 'inventory')
        compose('up', '-d', '--wait', '--wait-timeout', '120', 'inventory')
        assert call(inv, '/reservations/' + pending)[0] == saved
        compose('run', '--rm', 'inventory-import')
        assert available(pid) == before - 1
        print('PASS restart persistence and import replay: no stock overwrite', flush=True)
