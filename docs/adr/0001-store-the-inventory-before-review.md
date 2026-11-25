# Save the inventory before starting a coverage review

Codex needs the same revision and scanner output that produced the inventory. Sweep saves the inventory before invoking Codex and tracks the review outcome separately. Incomplete source extraction can itself provide evidence for review. If a complete inventory’s review fails, the next run can check out that revision and retry the review using the saved inventory.
