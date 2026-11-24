#!/usr/bin/env python3
"""Fail closed on same-state, full-residency size experiment associations."""
import json
from pathlib import Path
import sys


def validate(directory, output):
    directory, output = Path(directory), Path(output)
    observations = [json.loads(line) for line in (directory / "observations.jsonl").read_text().splitlines()]
    captures = [json.loads(line) for line in (directory / "capture-residency.jsonl").read_text().splitlines()]
    assert len(observations) == 3 and len(captures) == 2, "expected restore then exactly two captures"
    run = observations[0]["run_id"]
    pid = observations[0]["pid"]
    assert run and pid > 0
    size = 256 << 20
    for sequence, record in enumerate(observations, 1):
        assert (record["run_id"], record["pid"], record["sequence"], record["bytes"]) == (run, pid, sequence, size), record
        assert Path(record["path"]).parent == directory, "observation outside this run"
        assert Path(record["path"]).stat().st_size == size, "missing/truncated RAM observation"
    for number, (capture, name, observation) in enumerate(zip(captures, ("h", "c2"), observations[1:]), 1):
        assert (capture["run_id"], capture["pid"], capture["capture_sequence"], capture["resume_sequence"]) == (run, pid, number, number+1), capture
        assert capture["size"] == size and capture["holes"] == [], "dense baseline requires capture-time full M"
        compared = json.loads((output / (name + "-comparison.json")).read_text())
        result = json.loads((output / (name + ".json")).read_text())
        assert compared["complete_comparison"] == "equal" and compared["logical_bytes"] == size
        assert compared["sha256"] == observation["sha256"], "wrong frozen round"
        # This plaintext file carrier's first qualifier is its content identity.
        # Later location/runtime annotations must not replace that identity.
        assert result["snapshotRef"].split("@", 2)[1] == compared["layers"][0]["Identity"], "wrong snapshot identity"
        if name == "h":
            top = compared["layers"][0]
            assert top["Data"] == size and top["Zero"] == 0 and top["DirtyPresent"], "false top must preserve full W and its clean metadata"
            assert len(compared["layers"]) == 2, "expected exactly one owned H and no external suffix"
        else:
            assert len(compared["layers"]) == 1, "true self must absorb all local history with no external suffix"
            assert not compared["layers"][0]["DirtyPresent"], "true change-only self is implicitly all dirty"
    original = json.loads((output / "w2-comparison.json").read_text())
    assert original["layers"][0]["Data"] == size and original["layers"][0]["Zero"] == 0, "legacy H input must fully cover the owned history"
    return {"run_id": run, "pid": pid, "captures": 2, "memory_bytes": size, "observer_materializes_memory": True, "codec": "plaintext"}


def validate_loss_records(records, lost):
    assert len(records) == 4, 'two successes, dropped response, then recovered capture expected'
    first=records[0]
    assert first['run_id'] and first['pid'] > 0
    for i,record in enumerate(records,1):
        assert (record['run_id'],record['pid'],record['capture_sequence'],record['resume_sequence'],record['size']) == (first['run_id'],first['pid'],i,i+1,256<<20), 'response loss round mismatch'
    assert records[2]['dirty'] == lost, 'saved dropped response differs from collection record'
    return validate_loss(records[1]['dirty'],lost,records[3]['dirty'])


def validate_loss(previous, lost, recovered):
    def regions(report):
        assert (report['version'], report['page_shift']) == (1, 12)
        result = {}
        for r in report['regions']:
            assert r['slot'] not in result, 'duplicate slot'
            assert r['size'] > 0 and all(r[k] % 4096 == 0 for k in ('gpa', 'size', 'file_offset'))
            pages = r['size'] // 4096
            assert len(r['bitmap']) == (pages+63)//64
            assert all(isinstance(w,int) and 0 <= w < 1<<64 for w in r['bitmap'])
            if pages % 64: assert r['bitmap'][-1] >> (pages % 64) == 0
            result[r['slot']] = r
        assert result
        return result
    old, dropped, new = map(regions, (previous,lost,recovered))
    assert old.keys() == dropped.keys() == new.keys(), 'slot set changed'
    added = 0
    for slot, r in dropped.items():
        assert all(old[slot][k] == r[k] == new[slot][k] for k in ('gpa','size','file_offset')), 'geometry changed'
        for before, consumed, after in zip(old[slot]['bitmap'],r['bitmap'],new[slot]['bitmap']):
            assert before & ~consumed == 0, 'prior bits lost before response loss'
            assert consumed & ~after == 0, 'consumed dirty bits lost on retry'
            added += (consumed & ~before).bit_count()
    assert added > 0, 'lost response introduced no new dirty bits'
    return {'newly_consumed_lost_pages': added, 'retained_after_retry': True}


if __name__ == "__main__":
    print(json.dumps(validate(*sys.argv[1:])))
