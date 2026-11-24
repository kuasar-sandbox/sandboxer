#!/usr/bin/env python3
"""The size harness must reject missing, sparse, stale and mismatched evidence."""
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("evidence", Path(__file__).resolve().parents[1] / "test/e2e/lib/dirty_memory_evidence.py")
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)


class EvidenceTest(unittest.TestCase):
    def test_associations_fail_closed(self):
        for change in ("valid", "valid-runtime", "valid-location-runtime", "wrong-identity-runtime", "empty", "missing", "extra", "sparse", "wrong-pid", "wrong-round", "stale-run", "wrong-hash", "wrong-identity", "thin-false", "missing-bitmap", "true-extra-layer", "true-unexpected-bitmap"):
            with self.subTest(change=change), tempfile.TemporaryDirectory() as directory:
                out = Path(directory)
                ram = out / "ram"
                ram.mkdir()
                observations = []
                captures = []
                for seq in (1, 2, 3):
                    path = ram / f"{seq}.bin"
                    with path.open("wb") as file:
                        file.truncate(256 << 20)
                    observations.append(dict(run_id="fresh", pid=42, sequence=seq, bytes=256 << 20, path=str(path), sha256=f"hash-{seq}"))
                for seq, name in enumerate(("h", "c2"), 1):
                    captures.append(dict(run_id="fresh", pid=42, capture_sequence=seq, resume_sequence=seq+1, size=256 << 20, holes=[]))
                    top = dict(Data=256 << 20, Zero=0, DirtyPresent=name == "h", Identity=f"digest:{name}")
                    compared = dict(complete_comparison="equal", logical_bytes=256 << 20, sha256=f"hash-{seq+1}", layers=[top, copy.copy(top)] if name == "h" else [top])
                    if name == "h":
                        if change == "wrong-hash": compared["sha256"] = "wrong"
                        if change == "thin-false": top["Data"] -= 4096
                        if change == "missing-bitmap": top["DirtyPresent"] = False
                        if change in ("wrong-identity", "wrong-identity-runtime"): top["Identity"] = "digest:wrong"
                    if name == "c2":
                        if change == "true-extra-layer": compared["layers"].append(copy.copy(top))
                        if change == "true-unexpected-bitmap": top["DirtyPresent"] = True
                    (out / (name + "-comparison.json")).write_text(json.dumps(compared))
                    ref = f"file://{name}@digest:{name}"
                    if change == "valid-location-runtime":
                        ref += "@location:portable"
                    if change in ("valid-runtime", "valid-location-runtime", "wrong-identity-runtime"):
                        ref += "@runtime:sandbox-runtime.bundle:digest:" + "a" * 64
                    (out / (name + ".json")).write_text(json.dumps(dict(snapshotRef=ref)))
                (out / "w2-comparison.json").write_text(json.dumps(dict(layers=[dict(Data=256 << 20, Zero=0)])))
                if change == "empty": captures = []
                if change == "missing": captures.pop()
                if change == "extra": captures.append(captures[-1])
                if change == "sparse": captures[0]["holes"] = [[0,4096]]
                if change == "wrong-pid": captures[0]["pid"] += 1
                if change == "wrong-round": captures[0]["resume_sequence"] += 1
                if change == "stale-run": observations[0]["run_id"] = "stale"
                (ram / "observations.jsonl").write_text("\n".join(map(json.dumps, observations)))
                (ram / "capture-residency.jsonl").write_text("\n".join(map(json.dumps, captures)))
                if change in ("valid", "valid-runtime", "valid-location-runtime"):
                    self.assertEqual(evidence.validate(ram,out)["captures"],2)
                else:
                    with self.assertRaises(AssertionError): evidence.validate(ram,out)

    def test_loss_requires_new_bits_and_exact_geometry(self):
        def report(word):
            return dict(version=1,page_shift=12,regions=[dict(slot=0,gpa=0,size=4096*4,file_offset=0,bitmap=[word])])
        self.assertEqual(evidence.validate_loss(report(1),report(5),report(7))['newly_consumed_lost_pages'],1)
        for a,b,c in [(1,1,1),(1,5,1),(5,1,7)]:
            with self.assertRaises(AssertionError): evidence.validate_loss(report(a),report(b),report(c))
        for mutation in ('length','geometry','duplicate','slot','tail'):
            bad=report(7)
            if mutation=='length': bad['regions'][0]['bitmap']=[]
            if mutation=='geometry': bad['regions'][0]['gpa']=4096
            if mutation=='duplicate': bad['regions']*=2
            if mutation=='slot': bad['regions'][0]['slot']=1
            if mutation=='tail': bad['regions'][0]['bitmap']=[16]
            with self.assertRaises(AssertionError): evidence.validate_loss(report(1),report(5),bad)

    def test_loss_records_include_the_dropped_collection(self):
        report=lambda word: dict(version=1,page_shift=12,regions=[dict(slot=0,gpa=0,size=4096*4,file_offset=0,bitmap=[word])])
        records=[dict(run_id='same',pid=42,capture_sequence=i,resume_sequence=i+1,size=256<<20,dirty=report(word)) for i,word in enumerate((1,1,5,7),1)]
        self.assertEqual(evidence.validate_loss_records(records,report(5))['newly_consumed_lost_pages'],1)
        for kind in ('missing','extra','pid','run','round','lost'):
            bad=copy.deepcopy(records)
            lost=report(5)
            if kind=='missing': bad.pop(2)
            if kind=='extra': bad.append(bad[-1])
            if kind=='pid': bad[-1]['pid']+=1
            if kind=='run': bad[2]['run_id']='stale'
            if kind=='round': bad[2]['resume_sequence']+=1
            if kind=='lost': lost=report(7)
            with self.assertRaises(AssertionError): evidence.validate_loss_records(bad,lost)


if __name__ == "__main__":
    unittest.main()
