CREATE INDEX finance_unresolved_debt_spender ON finance_payment_attempts(user_id)
    WHERE action='bad_debt' AND state IN ('reserved','unknown');
