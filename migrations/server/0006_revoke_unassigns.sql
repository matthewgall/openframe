-- 0006: a revoked device can never sync again (revocation is terminal:
-- revoked devices cannot be issued credentials), so its album
-- assignments are dead weight — and confusing in the assignment UI
-- when a live frame shares the dead one's name. Clean up historical
-- assignments; the revoke endpoint itself unassigns from this point
-- on (api: handleRevokeDevice).

DELETE FROM album_devices
WHERE device_id IN (
    SELECT id FROM devices
    WHERE revoked_at IS NOT NULL AND revoked_at != ''
);
