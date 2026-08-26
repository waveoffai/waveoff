# The gate's statistics

The gate decides whether to ship. Getting it wrong in the permissive direction
ships a regression; getting it wrong in the conservative direction blocks good
releases until people route around the tool. Both are failures.

## Fixed assumptions

- **Non-inferiority, not superiority.** You almost never need "the candidate is
  better", you need "not worse by more than I can accept". There is deliberately
  no default margin: it is a judgement about someone's product.
- **One primary metric carries the full alpha.** Everything else is a guardrail.
  Gating eight metrics at 0.05 each is a ~34% chance of a spurious rollback.
- **Guardrails use an intersection–union test.** They are already conservative;
  do not add a Bonferroni correction on top — it is pure loss.
- **Alpha is two-sided; the test reads one end**, so a nominal 0.05 is a
  one-sided 0.025. Say so wherever a number is reported.
- **A live stage must use the sequential test.** The controller peeks
  continuously, and a fixed-horizon test peeked at repeatedly will eventually
  call a good candidate a regression. Measured: 12.5% false-regression rate
  under peeking versus 0.0% for the confidence sequence.
- **Pairing is a property of the deployment, not a setting.** Offline replay and
  shadow are paired; a live canary cannot be. The unpaired interval is ~1.8×
  wider on identical data — roughly three times the sessions for the same
  resolution.
- **Repeated measurements of one item are refused**, not accepted. Resampling
  them as independent understates variance and over-promotes. A clustered
  bootstrap is not implemented, so the shape that would silently break it is
  rejected.
- **Differential missingness is checked.** An item scored under one arm and not
  the other is evidence, and the discordant pairs are what carry it (McNemar),
  not the totals.

## Sufficiency is not a count

`MinimumItems` was wrong and was removed. Whether you have enough evidence is
whether the interval straddles the margin, not whether you have five items.
`BootstrapFloor` exists only because a bootstrap on a handful of points is
meaningless arithmetic.

## Before you change any of this

- State which assumption you are changing and what breaks if it is wrong.
- Add a test that fails under the old behaviour with data of a known effect
  size. Every statistical claim here has one; a change without one is a claim
  nobody checked.
- If the change makes the gate more permissive, say so explicitly in the PR.
  That is the direction that ships regressions.
