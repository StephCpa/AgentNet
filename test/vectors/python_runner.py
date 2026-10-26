import hashlib
import json
import math
import sys
import rfc8785

def pairs(items):
    result = {}
    for key, value in items:
        if key in result:
            raise ValueError('duplicate')
        result[key] = value
    return result

def number(raw):
    n = float(raw)
    if not math.isfinite(n) or (n.is_integer() and abs(n) >= 2**53):
        raise ValueError('number')
    return n

for line in sys.stdin:
    try:
        value = json.loads(line, object_pairs_hook=pairs, parse_float=number, parse_int=number, parse_constant=number)
        data = rfc8785.dumps(value)
        print(json.dumps(dict(ok=True, canonical=data.decode('utf-8'), digest='sha256:'+hashlib.sha256(data).hexdigest())))
    except (ValueError, TypeError, UnicodeError):
        print('{"ok":false}')
