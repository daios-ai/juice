-- Price Snapshot Pattern (§16): a catalog price is DERIVED from current components; a funding
-- boundary FREEZES the total and every input it was computed from.
--
-- actions.base_price is the seller's manifest price (mp) on a remote_proxy row — the catalog
-- component that lets the local total be recomputed whenever import_bps changes, instead of the
-- total being frozen at import and never repriced (§8). NULL = imported before this change; such a
-- row re-resolves before it is next funded and is never reverse-calculated from its rounded total.
ALTER TABLE actions ADD COLUMN base_price INTEGER;

-- steps.import_bps freezes the origin fee a Step was funded under. A Step parks its price at
-- creation and settles arbitrarily later, so the rate in force at completion is the wrong one:
-- CreateStep is a funding boundary. NULL = parked before this change; those settle as they do today.
ALTER TABLE steps ADD COLUMN import_bps INTEGER;
