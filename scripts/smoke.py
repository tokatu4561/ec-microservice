"""匿名カートと一括注文を実HTTPで確認。成功注文で2商品の在庫を1ずつ減らす。"""
import json
import os
from http.cookiejar import CookieJar
from urllib.error import HTTPError
from urllib.request import Request, build_opener, HTTPCookieProcessor

base = os.environ.get('BASE_URL', 'http://localhost:3000').rstrip('/')
client = build_opener(HTTPCookieProcessor(CookieJar()))
other = build_opener(HTTPCookieProcessor(CookieJar()))


def call(path, body=None, expected=200, method=None, browser=client):
    req = Request(base + path, data=None if body is None else json.dumps(body).encode(),
                  method=method, headers={'Content-Type': 'application/json', 'X-Cart-Request': '1'})
    try:
        response = browser.open(req, timeout=10)
    except HTTPError as error:
        response = error
    with response:
        payload = json.load(response)
        assert response.status == expected, (response.status, payload)
        assert response.headers.get('X-Request-ID')
        assert response.headers.get('Cache-Control') == 'no-store'
        return payload


products = [p for p in call('/api/products')['products'] if p['stock'] > 0][:2]
assert len(products) == 2, 'two stocked products required'
cart = call('/api/cart')['cart']
for p in products:
    cart = call('/api/cart/items/' + str(p['id']), {'version': cart['version'], 'quantity': 1}, method='PUT')['cart']
assert call('/api/cart')['cart'] == cart
assert call('/api/cart', browser=other)['cart']['items'] == []
call('/api/cart/checkout', {'version': 0}, 409)
for payment, shipping, reason in [('fail', 'success', 'payment_failed'), ('success', 'fail', 'shipping_failed'), ('success', 'success', None)]:
    order = call('/api/cart/checkout', {'version': cart['version'], 'paymentMode': payment, 'shippingMode': shipping}, 201)['order']
    assert len(order['items']) == 2 and order['failureReason'] == reason
    assert order['totalYen'] == sum(p['priceYen'] for p in products)
    assert call('/api/orders/' + order['id'])['order'] == order
    cart = call('/api/cart')['cart']
    assert len(cart['items']) == (0 if reason is None else 2)
    saved = {p['id']: p for p in call('/api/products')['products']}
    for p in products:
        assert saved[p['id']]['stock'] == p['stock'] - (reason is None)
    print('PASS cart checkout', reason or 'success', order['id'])
call('/api/cart/checkout', {'version': cart['version']}, 400)
p = products[0]
legacy = call('/api/orders', {'productId': p['id'], 'quantity': p['stock'] + 1}, 201)['order']
assert len(legacy['items']) == 1 and legacy['failureReason'] == 'out_of_stock'
call('/api/orders', {'productId': p['id'], 'quantity': 0}, 400)
print('PASS persistence, isolation, stale version, empty cart, legacy input and validation')
