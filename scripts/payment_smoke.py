"""Payment単体の実HTTP検証。--lifecycleはPayment・専用DBの再起動と永続性も確認する。"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen
from uuid import uuid4

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--lifecycle', action='store_true')
args = parser.parse_args()
base = os.environ.get('PAYMENT_BASE_URL', 'http://127.0.0.1:8081').rstrip('/')
root = Path(__file__).resolve().parents[1]
request_id = 'payment-smoke-' + uuid4().hex


def call(path, body=None, expected=200, method=None):
    req = Request(base + path, data=None if body is None else json.dumps(body).encode(),
                  method=method, headers={'Content-Type': 'application/json', 'X-Request-ID': request_id})
    try:
        response = urlopen(req, timeout=6)
    except HTTPError as error:
        response = error
    with response:
        payload = json.load(response)
        assert response.status == expected, (response.status, payload)
        assert response.headers.get('X-Request-ID') == request_id
        assert response.headers.get('Cache-Control') == 'no-store'
        return payload


def compose(*command):
    subprocess.run(['docker', 'compose', *command], cwd=root, check=True)


def wait_healthy():
    deadline = time.monotonic() + 60
    while True:
        try:
            call('/healthz')
            return
        except (URLError, TimeoutError, ConnectionError, AssertionError):
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.5)


call('/healthz')
order_id = uuid4().hex
payment = call('/payments', {'orderId': order_id, 'amountYen': 980}, 201)['payment']
assert payment['status'] == 'succeeded' and payment['amountYen'] == 980
assert call('/payments/' + order_id)['payment'] == payment
call('/payments', {'orderId': order_id, 'amountYen': 1, 'mode': 'fail'}, 409)
assert call('/payments/' + order_id)['payment'] == payment
cancelled = call('/payments/' + order_id + '/cancel', method='POST')['payment']
assert cancelled['status'] == 'cancelled'
assert cancelled['createdAt'] == payment['createdAt']
assert call('/payments/' + order_id + '/cancel', method='POST')['payment'] == cancelled
failed_id = uuid4().hex
failed = call('/payments', {'orderId': failed_id, 'amountYen': 320, 'mode': 'fail'}, 201)['payment']
assert failed['status'] == 'failed'
call('/payments/' + failed_id + '/cancel', expected=409, method='POST')
missing = uuid4().hex
call('/payments/' + missing, expected=404)
call('/payments/' + missing + '/cancel', expected=404, method='POST')
call('/payments', {'orderId': uuid4().hex, 'amountYen': -1}, 400)
print('PASS Payment create, lookup, duplicate, cancel, failure and validation', flush=True)
print('order_id=' + order_id + ' request_id=' + request_id, flush=True)

if args.lifecycle:
    try:
        compose('restart', 'payment-db', 'payment')
        # DBが再起動中にPaymentが起動失敗しても、依存先の準備完了後に起動する。
        compose('up', '-d', '--wait', '--wait-timeout', '120', 'payment')
        wait_healthy()
        assert call('/payments/' + order_id)['payment'] == cancelled
        assert call('/payments/' + failed_id)['payment'] == failed
        print('PASS Payment and DB restart preserve records', flush=True)
    finally:
        compose('up', '-d', '--wait', '--wait-timeout', '120', 'payment')
