"""画面と同じカートAPI・単品APIから実Paymentまで照合。--lifecycleはPayment停止も確認。"""
import argparse
import json
import os
from pathlib import Path
import subprocess
from http.cookiejar import CookieJar
from urllib.error import HTTPError
from urllib.request import Request, build_opener, HTTPCookieProcessor

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--lifecycle', action='store_true')
args = parser.parse_args()
base = os.environ.get('BASE_URL', 'http://127.0.0.1:3000').rstrip('/')
payment_base = os.environ.get('PAYMENT_BASE_URL', 'http://127.0.0.1:8081').rstrip('/')
client = build_opener(HTTPCookieProcessor(CookieJar()))
root = Path(__file__).resolve().parents[1]


def call(origin, path, body=None, expected=200, method=None):
    req = Request(origin + path, data=None if body is None else json.dumps(body).encode(), method=method,
                  headers={'Content-Type': 'application/json', 'X-Cart-Request': '1'})
    try:
        response = client.open(req, timeout=8)
    except HTTPError as error:
        response = error
    with response:
        result = json.load(response)
        assert response.status == expected, (path, response.status, result)
        return result, response.headers.get('X-Request-ID')


products = call(base, '/api/products')[0]['products']
product = next(p for p in products if p['stock'] >= 3)
cart = call(base, '/api/cart')[0]['cart']
cart = call(base, '/api/cart/items/' + str(product['id']),
            {'version': cart['version'], 'quantity': 1}, method='PUT')[0]['cart']
for pay, ship, status in [('fail', 'success', 'failed'), ('success', 'fail', 'cancelled'), ('success', 'success', 'succeeded')]:
    result, rid = call(base, '/api/cart/checkout',
                       {'version': cart['version'], 'paymentMode': pay, 'shippingMode': ship}, 201)
    order = result['order']
    payment = call(payment_base, '/payments/' + order['id'])[0]['payment']
    assert order['paymentStatus'] == payment['status'] == status
    assert order['totalYen'] == payment['amountYen'] == product['priceYen']
    assert order['id'] == payment['orderId']
    assert call(base, '/api/orders/' + order['id'])[0]['order'] == order
    cart = call(base, '/api/cart')[0]['cart']
    assert len(cart['items']) == (0 if status == 'succeeded' else 1)
    print('PASS cart -> Payment', status, 'order_id=' + order['id'], 'request_id=' + rid, flush=True)

result, rid = call(base, '/api/orders', {'productId': product['id'], 'quantity': 1}, 201)
order = result['order']
assert call(payment_base, '/payments/' + order['id'])[0]['payment']['status'] == 'succeeded'
print('PASS single -> Payment order_id=' + order['id'], 'request_id=' + rid, flush=True)

if args.lifecycle:
    cart = call(base, '/api/cart/items/' + str(product['id']),
                {'version': cart['version'], 'quantity': 1}, method='PUT')[0]['cart']
    before = call(base, '/api/products')[0]['products']
    order_id = None
    try:
        subprocess.run(['docker', 'compose', 'stop', 'payment'], cwd=root, check=True)
        result, rid = call(base, '/api/cart/checkout', {'version': cart['version']}, 503)
        order_id = result['orderId']
        assert result['requestId'] == rid
        pending = call(base, '/api/cart')[0]['cart']
        assert pending['pendingOrderId'] == pending['lastOrderId'] == order_id
        assert pending['version'] == cart['version'] + 1 and pending['items'][0]['quantity'] == 1
        after = call(base, '/api/products')[0]['products']
        assert next(p['stock'] for p in after if p['id'] == product['id']) == next(p['stock'] for p in before if p['id'] == product['id']) - 1
        assert call(base, '/api/orders/' + order_id)[0]['order']['status'] == 'processing'
        call(base, '/api/cart/checkout', {'version': pending['version']}, 409)
        subprocess.run(['docker', 'compose', 'restart', 'api'], cwd=root, check=True)
        print('PASS stopped Payment -> 503; order and reservation persisted; cart blocked; API restarted', 'order_id=' + order_id, flush=True)
    finally:
        subprocess.run(['docker', 'compose', 'up', '-d', '--wait', '--wait-timeout', '120', 'payment'], cwd=root, check=True)
    if order_id:
        call(payment_base, '/payments/' + order_id, expected=404)
        recovered = call(base, '/api/orders/' + order_id + '/resume', {})[0]['order']
        assert recovered['status'] == 'shipping_requested'
        assert call(base, '/api/orders/' + order_id + '/resume', {})[0]['order'] == recovered
        assert call(payment_base, '/payments/' + order_id)[0]['payment']['status'] == 'succeeded'
        recovered_cart = call(base, '/api/cart')[0]['cart']
        assert not recovered_cart['items'] and recovered_cart['pendingOrderId'] is None
    print('PASS Payment restored', flush=True)
