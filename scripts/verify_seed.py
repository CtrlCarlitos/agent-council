#!/usr/bin/env python3
"""Validate roadmap and policy structure, not immutable seed hashes, during normal CI."""
from publish import ROOT, validate_seed

if __name__ == '__main__':
    plan, issues = validate_seed(ROOT)
    print(f'Validated {len(issues)} issue specs, dependency DAG, labels/milestones and main policy.')
