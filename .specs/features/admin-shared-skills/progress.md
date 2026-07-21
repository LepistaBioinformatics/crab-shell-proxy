# admin-shared-skills — SDD progress (crab-shell-proxy)
Branch: feat/admin-shared-skills (from 7e3f58d)
- T1 config builders + skills.go store — DONE (5e1167b + fix 28bff80). 20/20 skill tests pass, zip-bomb Critical fixed+verified. Reviewed.
- T2/rework mounts — DONE (de65613 → reworked 4fc0928): live effective-dir mount at global root, mirrors secrets. Propagates via stop/start, no recreate.
- T3 http handlers + routes — DONE (2d64183). build+vet+httpapi tests pass, 5 routes registered.
Note: manager_test.go / TestEnsureRunning* fail in THIS sandbox (chown to uid 1000 = op not permitted) — PRE-EXISTING, not our code. Verify T2 via the pure effectiveSkills helper test (no chown).
- REWORK (skills live cascade) — DONE (4fc0928). EffectiveSkillsDir + syncEffectiveSkills, whole-dir RO mount at global root; RestartScope syncs skills too. build/gofmt/tests green.
- admin-shared-skills FEATURE COMPLETE (proxy). Webapp on feat/admin-shared-skills (b469d9f). Gateway routes in parent bcfc790.
