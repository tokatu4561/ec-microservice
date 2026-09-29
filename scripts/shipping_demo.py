"""Shipping分離のHTTPデモ。専用Compose環境で実行し、--lifecycleで停止・再起動からの復旧も確認する。"""
import argparse
import json
import os
import subprocess
import time
import uuid
from pathlib import Path
from urllib.request import Request, urlopen
from urllib.error import HTTPError

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--lifecycle', action='store_true')
args = parser.parse_args()
base = os.environ.get('BASE_URL', 'http://127.0.0.1:3000')
shipping = os.environ.get('SHIPPING_BASE_URL', 'http://127.0.0.1:8083')
payment = os.environ.get('PAYMENT_BASE_URL', 'http://127.0.0.1:8081')
jaeger = os.environ.get('JAEGER_URL', 'http://127.0.0.1:16686')
root = Path(__file__).resolve().parents[1]


def call(origin, path, body=None, expected=200):
    request = Request(origin + path, data=None if body is None else json.dumps(body).encode(),
                      headers={'Content-Type': 'application/json', 'X-Cart-Request': '1'})
    try:
        response = urlopen(request, timeout=8)
    except HTTPError as error:
        response = error
    with response:
        data = json.load(response)
        assert response.status in (expected if isinstance(expected, tuple) else (expected,)), (path, response.status, data)
        return data, response.headers.get('X-Trace-ID')


def compose(*arguments):
    subprocess.run(['docker', 'compose', *arguments], cwd=root, check=True)


def verify_trace(trace_id, required_services=None):
    required_services = required_services or {"order", "payment", "shipping"}
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        result, _ = call(jaeger, '/api/traces/' + trace_id, expected=(200, 404))
        if result.get('data'):
            trace = result['data'][0]
            services = {p['serviceName'] for p in trace['processes'].values()}
            spans = {s['spanID']: s for s in trace['spans']}
            linked = any(trace['processes'][s['processID']]['serviceName'] == 'shipping'
                         and any(r['spanID'] in spans and trace['processes'][spans[r['spanID']]['processID']]['serviceName'] == 'order'
                                 for r in s.get('references', [])) for s in spans.values())
            if required_services <= services and linked:
                print('PASS trace', jaeger + '/trace/' + trace_id, flush=True)
                return
        time.sleep(0.5)
    raise AssertionError('three-service trace or parent relationship missing: ' + trace_id)


products, _ = call(base, '/api/products')
product = next(p for p in products['products'] if p['stock'] >= 4)
for mode in ('success', 'fail'):
    data, trace_id = call(base, '/api/orders', {'productId': product['id'], 'quantity': 1, 'shippingMode': mode}, 201)
    order = data['order']
    shipment = call(shipping, '/shipments/' + order['id'])[0]['shipment']
    pay = call(payment, '/payments/' + order['id'])[0]['payment']
    assert shipment['status'] == ('requested' if mode == 'success' else 'failed')
    assert order['status'] == ('shipping_requested' if mode == 'success' else 'failed')
    assert pay['status'] == ('succeeded' if mode == 'success' else 'cancelled')
    replay = call(shipping, '/shipments', {'orderId': order['id'], 'items': shipment['items'], 'mode': mode}, 201)[0]['shipment']
    assert replay == shipment
    call(shipping, '/shipments', {'orderId': order['id'], 'items': [{'productId': product['id'], 'quantity': 2}], 'mode': mode}, 409)
    print('PASS', mode, 'order_id=' + order['id'], flush=True)
    verify_trace(trace_id)

# Standalone cancellation, deliberately unrelated to a completed order.
standalone_id = uuid.uuid4().hex
payload = {'orderId': standalone_id, 'items': [{'productId': product['id'], 'quantity': 1}]}
call(shipping, '/shipments', payload, 201)
# Cancellation API accepts an empty POST body, not a JSON object.
request = Request(shipping + '/shipments/' + standalone_id + '/cancel', data=b'', method='POST')
with urlopen(request, timeout=8) as response:
    cancelled = json.load(response)['shipment']
assert cancelled['status'] == 'cancelled'
assert call(shipping, '/shipments', payload, 201)[0]['shipment'] == cancelled
print('PASS standalone cancellation stays cancelled after replay', flush=True)

if args.lifecycle:
    order_id = None
    try:
        compose('stop', 'shipping')
        data, _ = call(base, '/api/orders', {'productId': product['id'], 'quantity': 1}, 503)
        order_id = data['orderId']
        order = call(base, '/api/orders/' + order_id)[0]['order']
        assert (order['status'], order['paymentStatus'], order['shippingStatus']) == ('processing', 'succeeded', 'pending')
        assert call(payment, '/payments/' + order_id)[0]['payment']['status'] == 'succeeded'
        compose('restart', 'api')
        print('PASS Shipping stopped: payment and stock reservation retained; Order restarted', order_id, flush=True)
    finally:
        compose('up', '-d', '--wait', '--wait-timeout', '120', 'shipping')
    if order_id:
        data, trace_id = call(base, '/api/orders/' + order_id + '/resume', {})
        assert data['order']['status'] == 'shipping_requested'
        assert call(base, '/api/orders/' + order_id + '/resume', {})[0] == data
        before = call(shipping, '/shipments/' + order_id)[0]
        compose('restart', 'shipping', 'shipping-db')
        compose('up', '-d', '--wait', '--wait-timeout', '120', 'shipping')
        assert call(shipping, '/shipments/' + order_id)[0] == before
        print('PASS resume and persistence order_id=' + order_id, flush=True)
        verify_trace(trace_id, {"order", "shipping", "inventory"})
