# tmstat parser test fixtures

`snap.tmm0.gz` is a gzip'd snapshot of a live tmm stats segment
(`/var/tmstat/blade/tmm0`), captured from the `debug` sidecar of an `f5-tmm`
pod. The `csv-*.csv` files are the **oracle**: `tmctl`'s own decode of that exact
snapshot. `TestCSVGoldens` asserts our pure-Go parser reproduces them byte for
byte — the gate that lets the exporter read tmstat without shipping `tmctl`.

`cols-*.txt` (tmctl `-C`) and `verify.txt` (tmctl `-V`) are kept as small
human-readable references for the segment layout the parser hardcodes.

## Regenerate

From a host with a running tmm pod (the `debug` sidecar carries `tmctl`):

```sh
POD=$(kubectl -n default get pod -l app=f5-tmm -o jsonpath='{.items[0].metadata.name}')
kubectl -n default exec "$POD" -c debug -- sh -c '
  cp /var/tmstat/blade/tmm0 /tmp/snap.tmm0
  tmctl -f /tmp/snap.tmm0 -V > /tmp/verify.txt 2>&1
  for T in tmm_stat virtual_server_stat pool_member_stat interface_stat; do
    tmctl -f /tmp/snap.tmm0 -c "$T" > /tmp/csv-$T.csv
    tmctl -f /tmp/snap.tmm0 -C "$T" > /tmp/cols-$T.txt
  done
  cd /tmp && tar czf /tmp/golden.tgz snap.tmm0 verify.txt csv-*.csv cols-*.txt'
kubectl -n default cp -c debug default/"$POD":/tmp/golden.tgz /tmp/golden.tgz
# extract here, then: gzip -9 snap.tmm0
```

The snapshot is lab stats only (counters + object names/IPs); it carries no
credentials.
