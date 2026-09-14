# /// script
# requires-python = ">=3.12,<3.13"
# dependencies = ["numpy", "faiss-cpu", "hnswlib", "sqlite-vec"]
# ///
"""Run SIFT1M through well-known vector search implementations for comparison
with TestSIFT1M in sift_test.go. Queries run single-threaded; index builds may
use all cores.

    uv run bench/sift_compare.py .tmp/sift 100
"""

import os
import sqlite3
import sys
import tempfile
import time

import faiss
import hnswlib
import numpy as np
import sqlite_vec

K = 100


def read_vecs(path, dtype):
    raw = np.fromfile(path, dtype=dtype)
    dim = raw[:1].view(np.int32)[0]
    return raw.reshape(-1, dim + 1)[:, 1:].copy()


def report(name, build, search):
    lat, hits1, hits10, hits100 = [], 0.0, 0.0, 0.0
    for q, t in zip(queries, truth):
        start = time.perf_counter()
        ids = search(q)
        lat.append(time.perf_counter() - start)
        ids = list(ids)
        hits1 += len(set(ids[:1]) & set(t[:1]))
        hits10 += len(set(ids[:10]) & set(t[:10])) / 10
        hits100 += len(set(ids[:100]) & set(t[:100])) / 100
    lat = np.array(lat)
    n = len(queries)
    print(
        f"{name:<28} {build:>9.1f}s {np.percentile(lat, 50) * 1e3:>9.2f}ms "
        f"{np.percentile(lat, 99) * 1e3:>9.2f}ms {n / lat.sum():>10.1f} "
        f"{hits1 / n:>7.4f} {hits10 / n:>7.4f} {hits100 / n:>7.4f}",
        flush=True,
    )


def timed(fn):
    start = time.perf_counter()
    result = fn()
    return result, time.perf_counter() - start


sift_dir, nq = sys.argv[1], int(sys.argv[2])
base = read_vecs(os.path.join(sift_dir, "sift_base.fvecs"), np.float32)
queries = read_vecs(os.path.join(sift_dir, "sift_query.fvecs"), np.float32)[:nq]
truth = read_vecs(os.path.join(sift_dir, "sift_groundtruth.ivecs"), np.int32)[:nq]
dim = base.shape[1]

print(f"{'method':<28} {'build':>10} {'p50':>11} {'p99':>11} {'QPS':>10} {'R@1':>7} {'R@10':>7} {'R@100':>7}")

# FAISS exact (BLAS brute force).
faiss.omp_set_num_threads(1)
flat = faiss.IndexFlatL2(dim)
_, build = timed(lambda: flat.add(base))
report("faiss IndexFlatL2", build, lambda q: flat.search(q[None], K)[1][0])
del flat

# FAISS HNSW.
faiss.omp_set_num_threads(os.cpu_count())
hnsw = faiss.IndexHNSWFlat(dim, 16)
hnsw.hnsw.efConstruction = 200
_, build = timed(lambda: hnsw.add(base))
faiss.omp_set_num_threads(1)
for ef in (128, 512):
    hnsw.hnsw.efSearch = ef
    report(f"faiss HNSW M=16 ef={ef}", build, lambda q: hnsw.search(q[None], K)[1][0])
del hnsw

# hnswlib.
hl = hnswlib.Index(space="l2", dim=dim)
hl.init_index(max_elements=len(base), M=16, ef_construction=200)
hl.set_num_threads(os.cpu_count())
_, build = timed(lambda: hl.add_items(base, np.arange(len(base))))
hl.set_num_threads(1)
for ef in (128, 512):
    hl.set_ef(ef)
    report(f"hnswlib M=16 ef={ef}", build, lambda q: hl.knn_query(q, k=K)[0][0])
del hl

# sqlite-vec, both as a plain-table scan with a scalar distance function (the
# same query shape as this library) and via its vec0 virtual table.
with tempfile.TemporaryDirectory() as tmp:
    db = sqlite3.connect(os.path.join(tmp, "sift.db"))
    db.enable_load_extension(True)
    sqlite_vec.load(db)

    def load_plain():
        db.execute("CREATE TABLE sift (id INTEGER PRIMARY KEY, embedding BLOB)")
        db.executemany("INSERT INTO sift VALUES (?, ?)", ((i, v.tobytes()) for i, v in enumerate(base)))
        db.commit()

    _, build = timed(load_plain)
    report(
        "sqlite-vec scalar scan",
        build,
        lambda q: [r[0] for r in db.execute(
            "SELECT id FROM sift ORDER BY vec_distance_l2(embedding, ?) LIMIT 100", (q.tobytes(),))],
    )

    def load_vec0():
        db.execute(f"CREATE VIRTUAL TABLE sift_vec0 USING vec0(embedding float[{dim}])")
        db.executemany(
            "INSERT INTO sift_vec0 (rowid, embedding) VALUES (?, ?)", ((i, v.tobytes()) for i, v in enumerate(base)))
        db.commit()

    _, build = timed(load_vec0)
    report(
        "sqlite-vec vec0",
        build,
        lambda q: [r[0] for r in db.execute(
            "SELECT rowid FROM sift_vec0 WHERE embedding MATCH ? AND k = 100", (q.tobytes(),))],
    )
    db.close()
