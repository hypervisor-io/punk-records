-- Feedback weight (cognee-borrow): a user rating on an answer EWMA-updates
-- the weight of the facts that were used, folded back into ranking.
ALTER TABLE memories ADD COLUMN feedback_weight REAL NOT NULL DEFAULT 0.5;
