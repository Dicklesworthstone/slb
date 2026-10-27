-- Version 7 adds assurance metadata without rewriting old journal payloads or
-- changing cursor anchors. This runs after the policy columns are added, in
-- the same migration transaction. Existing events remain legacy observations.
DROP VIEW request_event_projection;
CREATE VIEW request_event_projection AS
SELECT r.id AS request_id, r.project_path,
  json_object('state', json_object(
    'approval_policy', r.approval_policy,
    'execution_approval_policy', r.execution_approval_policy,
    'execution_assurance', r.execution_assurance,
    'command_hash', r.command_hash,
    'status', r.status,
    'risk_tier', r.risk_tier,
    'requestor_session_id', r.requestor_session_id,
    'min_approvals', r.min_approvals,
    'require_different_model', json(CASE WHEN r.require_different_model != 0 THEN 'true' ELSE 'false' END),
    'approvals', (SELECT COUNT(*) FROM reviews v WHERE v.request_id = r.id AND v.decision = 'approve'),
    'rejections', (SELECT COUNT(*) FROM reviews v WHERE v.request_id = r.id AND v.decision = 'reject'),
    'created_at', r.created_at,
    'expires_at', r.expires_at,
    'resolved_at', r.resolved_at,
    'approval_expires_at', r.approval_expires_at,
    'executor_session_id', r.execution_executed_by_session_id,
    'executed_at', r.execution_executed_at,
    'exit_code', r.execution_exit_code,
    'duration_ms', r.execution_duration_ms,
    'rolled_back_at', r.rollback_rolled_back_at
  )) AS payload
FROM requests r;

DROP TRIGGER request_events_update;
CREATE TRIGGER request_events_update AFTER UPDATE ON requests
WHEN old.id IS NOT new.id OR old.project_path IS NOT new.project_path
  OR old.status IS NOT new.status OR old.command_hash IS NOT new.command_hash
  OR old.command_raw IS NOT new.command_raw OR old.command_argv_json IS NOT new.command_argv_json
  OR old.command_cwd IS NOT new.command_cwd OR old.command_shell IS NOT new.command_shell
  OR old.command_display_redacted IS NOT new.command_display_redacted
  OR old.command_contains_sensitive IS NOT new.command_contains_sensitive
  OR old.risk_tier IS NOT new.risk_tier OR old.min_approvals IS NOT new.min_approvals
  OR old.require_different_model IS NOT new.require_different_model
  OR old.approval_policy IS NOT new.approval_policy
  OR old.execution_approval_policy IS NOT new.execution_approval_policy
  OR old.execution_assurance IS NOT new.execution_assurance
  OR old.requestor_session_id IS NOT new.requestor_session_id
  OR old.requestor_agent IS NOT new.requestor_agent OR old.requestor_model IS NOT new.requestor_model
  OR old.created_at IS NOT new.created_at OR old.expires_at IS NOT new.expires_at
  OR old.resolved_at IS NOT new.resolved_at OR old.approval_expires_at IS NOT new.approval_expires_at
  OR old.execution_exit_code IS NOT new.execution_exit_code
  OR old.execution_duration_ms IS NOT new.execution_duration_ms
  OR old.execution_executed_at IS NOT new.execution_executed_at
  OR old.execution_executed_by_session_id IS NOT new.execution_executed_by_session_id
  OR old.execution_executed_by_agent IS NOT new.execution_executed_by_agent
  OR old.execution_executed_by_model IS NOT new.execution_executed_by_model
  OR old.execution_log_path IS NOT new.execution_log_path
  OR old.rollback_path IS NOT new.rollback_path OR old.rollback_rolled_back_at IS NOT new.rollback_rolled_back_at
  OR old.justification_reason IS NOT new.justification_reason
  OR old.justification_expected_effect IS NOT new.justification_expected_effect
  OR old.justification_goal IS NOT new.justification_goal
  OR old.justification_safety_argument IS NOT new.justification_safety_argument
  OR old.dry_run_command IS NOT new.dry_run_command OR old.dry_run_output IS NOT new.dry_run_output
  OR old.attachments_json IS NOT new.attachments_json
BEGIN
  INSERT INTO request_events(kind, project_path, request_id, payload)
  SELECT CASE WHEN old.status IS NOT new.status THEN 'request_status_changed' ELSE 'request_updated' END,
    project_path, request_id, json_set(payload, '$.previous_status', old.status)
  FROM request_event_projection WHERE request_id = new.id;
END;

DROP TRIGGER request_events_review_insert;
CREATE TRIGGER request_events_review_insert AFTER INSERT ON reviews BEGIN
  INSERT INTO request_events(kind, project_path, request_id, payload)
  SELECT 'review_added', project_path, request_id,
    json_set(payload, '$.review', json_object('id', new.id, 'session_id', new.reviewer_session_id,
      'decision', new.decision, 'approval_policy', new.approval_policy, 'assurance', new.assurance,
      'command_hash', new.command_hash, 'signature_version', new.signature_version))
  FROM request_event_projection WHERE request_id = new.request_id;
END;

DROP TRIGGER request_events_review_update;
CREATE TRIGGER request_events_review_update AFTER UPDATE ON reviews
WHEN old.id IS NOT new.id OR old.request_id IS NOT new.request_id
  OR old.reviewer_session_id IS NOT new.reviewer_session_id
  OR old.reviewer_agent IS NOT new.reviewer_agent OR old.reviewer_model IS NOT new.reviewer_model
  OR old.decision IS NOT new.decision OR old.signature IS NOT new.signature
  OR old.signature_timestamp IS NOT new.signature_timestamp OR old.created_at IS NOT new.created_at
  OR old.responses_json IS NOT new.responses_json OR old.comments IS NOT new.comments
  OR old.approval_policy IS NOT new.approval_policy OR old.assurance IS NOT new.assurance
  OR old.command_hash IS NOT new.command_hash OR old.signature_version IS NOT new.signature_version
  OR old.request_binding IS NOT new.request_binding
BEGIN
  INSERT INTO request_events(kind, project_path, request_id, payload)
  SELECT 'review_updated', project_path, request_id,
    json_set(payload, '$.review', json_object('id', new.id, 'session_id', new.reviewer_session_id,
      'decision', new.decision, 'approval_policy', new.approval_policy, 'assurance', new.assurance,
      'command_hash', new.command_hash, 'signature_version', new.signature_version))
  FROM request_event_projection WHERE request_id = new.request_id;
  INSERT INTO request_events(kind, project_path, request_id, payload)
  SELECT 'review_removed', project_path, request_id,
    json_set(payload, '$.review', json_object('id', old.id, 'session_id', old.reviewer_session_id,
      'decision', old.decision, 'approval_policy', old.approval_policy, 'assurance', old.assurance,
      'command_hash', old.command_hash, 'signature_version', old.signature_version))
  FROM request_event_projection WHERE request_id = old.request_id AND old.request_id IS NOT new.request_id;
END;

DROP TRIGGER request_events_review_delete;
CREATE TRIGGER request_events_review_delete AFTER DELETE ON reviews BEGIN
  INSERT INTO request_events(kind, project_path, request_id, payload)
  SELECT 'review_removed', project_path, request_id,
    json_set(payload, '$.review', json_object('id', old.id, 'session_id', old.reviewer_session_id,
      'decision', old.decision, 'approval_policy', old.approval_policy, 'assurance', old.assurance,
      'command_hash', old.command_hash, 'signature_version', old.signature_version))
  FROM request_event_projection WHERE request_id = old.request_id;
END;
