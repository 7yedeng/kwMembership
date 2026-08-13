package processor

// productPriceEvidenceCardWhereSQL is deliberately broader than the existing-card
// reservation filter. A capacity-full card that was automatically frozen remains
// valid evidence for the price of opening another card of the same product, but it
// must never become reservable again.
const productPriceEvidenceCardWhereSQL = `c.reconciliation_state='READY' AND (
  c.upstream_status='ACTIVE'
  OR (c.upstream_status='FROZEN' AND c.capacity_state='CAPACITY_FULL')
)`
