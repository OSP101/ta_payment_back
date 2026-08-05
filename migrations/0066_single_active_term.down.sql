-- Dropping the index restores the old permissiveness. The demotions it forced
-- are not undone: there is no record of which extra terms used to be active,
-- and re-activating a guess would be worse than leaving one current term.
DROP INDEX IF EXISTS ux_academic_terms_single_active;
