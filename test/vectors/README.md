# JCS interoperability gate

Three independent implementations produce exactly the same canonical UTF-8 bytes
and SHA-256, or all reject invalid input:

| Language | Implementation | Pin |
|---|---|---|
| Go | [cyberphone/json-canonicalization](https://github.com/cyberphone/json-canonicalization) | go.mod pseudo-version ending 32a48195b9b2 |
| Python | [Trail of Bits rfc8785.py](https://github.com/trailofbits/rfc8785.py) | rfc8785 0.1.4 |
| Node | [erdtman/canonicalize](https://github.com/erdtman/canonicalize) | canonicalize 5.1.0; Node 22+ |

```text
python -m pip install -r test/vectors/requirements.txt --target .local/jcs-deps
npm ci --prefix test/vectors --ignore-scripts --no-audit --no-fund
python test/vectors/run.py --python-deps .local/jcs-deps
```

The harness runs 20 raw JSON cases: six existing vectors, UTF-16 key order,
negative zero/exponent formatting, IEEE-754 fraction rounding, root scalars/arrays,
duplicate/escaped/nested duplicate keys, unsafe integers, overflow/NaN, lone
surrogates and trailing JSON. Expected bytes are fixed independently of the
implementations; every process must exit successfully and return exactly one result
per case. Python/Node wrappers enforce the raw-input profile before serialization.

AgentNet rejects integer-valued JSON numbers with absolute value >= 2^53, even when
a particular larger value is exactly representable. Money is carried as canonical
decimal strings and validated separately by the TCS amount parser. This profile is
stricter than generic JCS; these 20 cases are a regression gate, not exhaustive RFC
conformance certification. Go also has an invalid-UTF-8 byte-level unit test.

2026-10-27: all 20 cases passed in each language (60 comparisons). Tested runtimes:
Go 1.25.1, Python 3.10, Node 22.20.0. Results are saved under
`test/evidence/2026-10-27/kernel/jcs-results.json`.
