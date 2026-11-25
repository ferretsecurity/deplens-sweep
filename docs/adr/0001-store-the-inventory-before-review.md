# Save the inventory before starting a coverage review

Codex needs the same revision and scanner output that produced the inventory. Sweep saves a complete inventory before invoking Codex and tracks the review outcome separately. If the review fails, the next run can check out that revision and retry the review using the saved inventory.
