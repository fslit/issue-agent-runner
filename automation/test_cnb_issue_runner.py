from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from automation import cnb_issue_runner as runner


def run_git(repo: Path, *args: str) -> str:
    result = subprocess.run(
        ["git", "-C", str(repo), *args],
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    return result.stdout.strip()


class CnbIssueRunnerTest(unittest.TestCase):
    def test_parse_cnb_remote_extracts_token_and_repo_path(self):
        token, repo_path = runner.parse_cnb_remote("https://cnb:secret-token@cnb.cool/dianyuniot/mlink/ml-svc-zcode.git")

        self.assertEqual(token, "secret-token")
        self.assertEqual(repo_path, "dianyuniot/mlink/ml-svc-zcode")

    def test_filter_processable_issues_skips_done_and_running_labels(self):
        issues = [
            {"number": "1", "title": "done", "labels": [{"name": "已处理"}]},
            {"number": "2", "title": "running", "labels": [{"name": "处理中"}]},
            {"number": "3", "title": "todo", "labels": []},
        ]

        filtered = runner.filter_processable_issues(issues, {"已处理", "处理中"})

        self.assertEqual([issue["number"] for issue in filtered], ["3"])

    def test_labels_for_inactive_running_success_removes_running_label(self):
        issue = {"number": "221", "labels": [{"name": "处理中"}, {"name": "ai:in-progress"}, {"name": "priority:P1"}]}
        meta = {"lastExitCode": 0, "lastFinishedAt": 200}

        labels = runner.labels_for_inactive_running_issue(issue, meta)

        self.assertEqual(labels, ["priority:P1"])

    def test_labels_for_paused_resource_error_requires_human(self):
        issue = {"number": "222", "labels": [{"name": "处理中"}]}
        meta = {
            "paused": True,
            "lastFinishedAt": 200,
            "lastError": "ERROR: unexpected status 403 Forbidden: 订阅额度不足",
        }

        labels = runner.labels_for_inactive_running_issue(issue, meta)

        self.assertEqual(labels, ["需人工确认"])

    def test_default_skip_labels_include_running_and_failed_states(self):
        self.assertIn("已处理", runner.DEFAULT_SKIP_LABELS)
        self.assertIn("处理中", runner.DEFAULT_SKIP_LABELS)
        self.assertIn("处理失败", runner.DEFAULT_SKIP_LABELS)
        self.assertIn("需人工确认", runner.DEFAULT_SKIP_LABELS)

    def test_default_retry_backoffs_reduce_ai_retry_churn(self):
        self.assertEqual(runner.DEFAULT_RETRY_BACKOFF_SECONDS, 600)
        self.assertEqual(runner.DEFAULT_CAPACITY_BACKOFF_SECONDS, 1800)
        self.assertEqual(runner.DEFAULT_PUSH_RETRY_BACKOFF_SECONDS, 600)

    def test_default_max_concurrency_is_conservative_for_shared_workspace(self):
        self.assertEqual(runner.DEFAULT_MAX_CONCURRENCY, 2)

    def test_format_log_line_prefixes_timestamp(self):
        line = runner.format_log_line("poll complete: open=1 candidates=0 capacity=7", now=0)

        self.assertRegex(line, r"^\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} .+\] poll complete:")

    def test_sanitized_subprocess_env_removes_malloc_debug_variables(self):
        env = {
            "PATH": "/bin",
            "MallocStackLogging": "0",
            "MallocStackLoggingNoCompact": "1",
            "MallocNanoZone": "0",
        }

        sanitized = runner.sanitized_subprocess_env(env)

        self.assertEqual(sanitized, {"PATH": "/bin"})

    def test_worker_slot_counts_reports_processing_and_idle_slots(self):
        state = {"workers": {"145": {"pid": 145}, "146": {"pid": 146}, "147": {"pid": 147}}}

        processing, idle = runner.worker_slot_counts(state, max_concurrency=7)

        self.assertEqual(processing, 3)
        self.assertEqual(idle, 4)

    def test_format_poll_complete_message_includes_processing_and_idle_counts(self):
        state = {"workers": {"145": {"pid": 145}, "146": {"pid": 146}}}

        message = runner.format_poll_complete_message(
            open_count=32,
            candidate_count=4,
            state=state,
            max_concurrency=7,
        )

        self.assertIn("poll complete: open=32 candidates=4", message)
        self.assertIn("capacity=5", message)
        self.assertIn("processing=2", message)
        self.assertIn("idle=5", message)

    def test_worker_prompt_requires_changed_files_with_lines_and_symbols(self):
        prompt = runner.build_worker_prompt(
            {
                "number": "72",
                "title": "field output label",
                "body": "body",
            },
            "dianyuniot/mlink/ml-svc-zcode",
        )

        self.assertIn("修改文件清单", prompt)
        self.assertIn("文件路径", prompt)
        self.assertIn("行号", prompt)
        self.assertIn("方法名", prompt)
        self.assertIn("新增方法名", prompt)
        self.assertIn("处理模型：gpt-5.5 / xhigh", prompt)
        self.assertIn("git 提交", prompt)
        self.assertIn("fix: resolve CNB issue #72", prompt)
        self.assertIn("中文简短问题描述", prompt)
        self.assertIn("问题描述必须使用中文", prompt)
        self.assertIn("commit body 必须使用中文描述", prompt)
        self.assertIn("修复内容:", prompt)
        self.assertIn("commit SHA", prompt)
        self.assertIn("runner 会在任务进入终态后自动 push", prompt)
        self.assertIn("所有 CNB 评论、任务回复、阻塞说明和完成说明都必须使用中文", prompt)

    def test_issue_is_terminal_for_done_label_or_closed_state(self):
        self.assertTrue(runner.issue_is_terminal({"number": "7", "state": "closed", "labels": []}))
        self.assertTrue(runner.issue_is_terminal({"number": "7", "state": "open", "labels": [{"name": "已处理"}]}))
        self.assertFalse(runner.issue_is_terminal({"number": "7", "state": "open", "labels": [{"name": "处理中"}]}))

    def test_render_pushed_comment_lists_pushed_repos_and_commits(self):
        comment = runner.render_pushed_comment(
            "7",
            [
                runner.PushedRepo(
                    name="ml-svc-zcode",
                    branch="ai-v2.0",
                    head_sha="abc123",
                    commits=["def456"],
                )
            ],
        )

        self.assertIn("Issue #7", comment)
        self.assertIn("ml-svc-zcode", comment)
        self.assertIn("ai-v2.0", comment)
        self.assertIn("abc123", comment)
        self.assertIn("def456", comment)

    def test_push_issue_commits_pushes_matching_unpushed_commit(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            origin = root / "origin.git"
            subprocess.run(["git", "init", "--bare", str(origin)], check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            subprocess.run(["git", "clone", str(origin), str(root / "ml-svc-zcode")], check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            repo = root / "ml-svc-zcode"
            run_git(repo, "config", "user.name", "Runner Test")
            run_git(repo, "config", "user.email", "runner@example.invalid")
            (repo / "README.md").write_text("initial\n", encoding="utf-8")
            run_git(repo, "add", "README.md")
            run_git(repo, "commit", "-m", "chore: initial")
            run_git(repo, "push", "-u", "origin", "HEAD")

            (repo / "README.md").write_text("initial\nfix\n", encoding="utf-8")
            run_git(repo, "add", "README.md")
            run_git(
                repo,
                "commit",
                "-m",
                "fix: resolve CNB issue #123 修复测试提交",
                "-m",
                "修复内容: 更新测试文件内容\n\n验证: 本地单测通过",
            )
            issue_commit = run_git(repo, "rev-parse", "HEAD")

            self.assertEqual(runner.unpushed_issue_commits(repo, "123"), [issue_commit])
            pushed = runner.push_issue_commits(root, "123")

            self.assertEqual(len(pushed), 1)
            self.assertEqual(pushed[0].name, "ml-svc-zcode")
            self.assertEqual(pushed[0].commits, [issue_commit])
            self.assertEqual(runner.unpushed_issue_commits(repo, "123"), [])

    def test_push_terminal_issues_pushes_new_commit_after_previous_push(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            origin = root / "origin.git"
            subprocess.run(["git", "init", "--bare", str(origin)], check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            subprocess.run(["git", "clone", str(origin), str(root / "ml-svc-zcode")], check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            repo = root / "ml-svc-zcode"
            run_git(repo, "config", "user.name", "Runner Test")
            run_git(repo, "config", "user.email", "runner@example.invalid")
            (repo / "README.md").write_text("initial\n", encoding="utf-8")
            run_git(repo, "add", "README.md")
            run_git(repo, "commit", "-m", "chore: initial")
            run_git(repo, "push", "-u", "origin", "HEAD")

            (repo / "README.md").write_text("initial\nfirst fix\n", encoding="utf-8")
            run_git(repo, "add", "README.md")
            run_git(repo, "commit", "-m", "fix: resolve CNB issue #190 第一次修复")
            first_commit = run_git(repo, "rev-parse", "HEAD")
            run_git(repo, "push", "origin", "HEAD")

            (repo / "README.md").write_text("initial\nfirst fix\nsecond fix\n", encoding="utf-8")
            run_git(repo, "add", "README.md")
            run_git(repo, "commit", "-m", "fix: resolve CNB issue #190 第二次修复")
            second_commit = run_git(repo, "rev-parse", "HEAD")

            state = {
                "issues": {
                    "190": {
                        "lastExitCode": 1,
                        "lastError": "ERROR: unexpected status 503 Service Unavailable",
                        "pushCompletedAt": 100,
                        "pushedRepos": [
                            {
                                "name": "ml-svc-zcode",
                                "branch": "master",
                                "headSha": first_commit,
                                "commits": [first_commit],
                            }
                        ],
                    }
                },
                "workers": {},
            }
            issues = []

            with (
                patch.object(runner, "get_issue", return_value={"number": "190", "state": "closed", "labels": []}) as get_issue,
                patch.object(runner, "add_issue_comment") as add_issue_comment,
            ):
                runner.push_terminal_issues(root, "token", "owner/repo", issues, state, now=200)

            self.assertEqual(runner.unpushed_issue_commits(repo, "190"), [])
            pushed_repos = state["issues"]["190"]["pushedRepos"]
            self.assertEqual(len(pushed_repos), 2)
            self.assertEqual(pushed_repos[0]["commits"], [first_commit])
            self.assertEqual(pushed_repos[1]["commits"], [second_commit])
            self.assertEqual(state["issues"]["190"]["pushCompletedAt"], 200)
            get_issue.assert_called_once_with("token", "owner/repo", "190")
            add_issue_comment.assert_called_once()

    def test_unpushed_issue_commits_ignores_equivalent_cherry_picked_remote_commit(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            origin = root / "origin.git"
            subprocess.run(["git", "init", "--bare", str(origin)], check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            subprocess.run(["git", "clone", str(origin), str(root / "local")], check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            local = root / "local"
            run_git(local, "config", "user.name", "Runner Test")
            run_git(local, "config", "user.email", "runner@example.invalid")
            (local / "README.md").write_text("initial\n", encoding="utf-8")
            run_git(local, "add", "README.md")
            run_git(local, "commit", "-m", "chore: initial")
            run_git(local, "push", "-u", "origin", "HEAD")

            (local / "README.md").write_text("initial\nfix\n", encoding="utf-8")
            run_git(local, "add", "README.md")
            run_git(local, "commit", "-m", "fix: resolve CNB issue #194 等价修复")
            local_commit = run_git(local, "rev-parse", "HEAD")

            subprocess.run(["git", "clone", str(origin), str(root / "remote")], check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            remote = root / "remote"
            run_git(remote, "config", "user.name", "Runner Test")
            run_git(remote, "config", "user.email", "runner@example.invalid")
            (remote / "README.md").write_text("initial\nfix\n", encoding="utf-8")
            run_git(remote, "add", "README.md")
            run_git(remote, "commit", "-m", "fix: resolve CNB issue #194 等价修复")
            run_git(remote, "push", "origin", "HEAD")
            run_git(local, "fetch", "origin")

            self.assertEqual(runner.unpushed_issue_commits(local, "194"), [])

    def test_push_terminal_issues_backs_off_after_push_failure(self):
        state = {
            "issues": {
                "191": {
                    "lastExitCode": 0,
                }
            },
            "workers": {},
        }
        issues = [{"number": "191", "state": "closed", "labels": []}]

        with patch.object(runner, "push_issue_commits", side_effect=RuntimeError("fetch first")) as push_issue_commits:
            runner.push_terminal_issues(Path("/tmp/workspace"), "token", "owner/repo", issues, state, now=300)

        meta = state["issues"]["191"]
        self.assertEqual(meta["pushLastAttemptAt"], 300)
        self.assertEqual(meta["nextPushRetryAt"], 900)
        self.assertIn("fetch first", meta["pushError"])
        push_issue_commits.assert_called_once()

        with patch.object(runner, "push_issue_commits") as push_issue_commits:
            candidates = runner.terminal_issue_candidates(
                issues,
                state,
                "token",
                "owner/repo",
                now=899,
                has_unpushed_commits=lambda _number: True,
            )

        self.assertEqual(candidates, {})
        push_issue_commits.assert_not_called()

    def test_terminal_issue_candidates_skips_completed_issue_without_new_unpushed_commits(self):
        state = {
            "issues": {
                "192": {
                    "lastExitCode": 0,
                    "pushCompletedAt": 200,
                }
            },
            "workers": {},
        }
        issues = [{"number": "192", "state": "closed", "labels": []}]

        candidates = runner.terminal_issue_candidates(
            issues,
            state,
            "token",
            "owner/repo",
            now=300,
            has_unpushed_commits=lambda _number: False,
        )

        self.assertEqual(candidates, {})

    def test_build_codex_command_passes_model_and_reasoning_effort(self):
        args = runner.parse_args(
            [
                "--workspace",
                "/tmp/zcode",
                "--codex-bin",
                "codex",
                "--codex-model",
                "gpt-5.5",
                "--codex-reasoning-effort",
                "xhigh",
            ]
        )

        command = runner.build_codex_command(args)

        self.assertIn("--model", command)
        self.assertIn("gpt-5.5", command)
        self.assertIn("-c", command)
        self.assertIn('model_reasoning_effort="xhigh"', command)
        self.assertEqual(command[-2:], ["--skip-git-repo-check", "-"])

    def test_prune_active_workers_keeps_only_live_pids(self):
        state = {
            "workers": {
                "1": {"pid": 101},
                "2": {"pid": 202},
                "3": {"pid": None},
            }
        }

        active = runner.prune_active_workers(state, pid_exists=lambda pid: pid == 101)

        self.assertEqual(active, {"1": {"pid": 101}})
        self.assertEqual(state["workers"], {"1": {"pid": 101}})

    def test_prune_active_workers_records_dead_worker_failure_and_backoff(self):
        state = {
            "workers": {
                "7": {
                    "pid": 707,
                    "log": "/tmp/issue-7.log",
                    "startedAt": 100,
                    "title": "capacity failure",
                }
            },
            "issues": {},
        }

        active = runner.prune_active_workers(
            state,
            pid_exists=lambda _pid: False,
            now=200,
            read_worker_result=lambda _worker: runner.WorkerResult(
                exit_code=1,
                error="ERROR: Selected model is at capacity. Please try a different model.",
            ),
            max_attempts=3,
            retry_backoff_seconds=300,
            capacity_backoff_seconds=900,
        )

        self.assertEqual(active, {})
        meta = state["issues"]["7"]
        self.assertEqual(meta["attempts"], 1)
        self.assertEqual(meta["lastExitCode"], 1)
        self.assertIn("Selected model is at capacity", meta["lastError"])
        self.assertEqual(meta["nextRetryAt"], 1100)
        self.assertFalse(meta.get("paused", False))

    def test_resource_errors_use_longer_backoff(self):
        state = {
            "workers": {
                "7": {
                    "pid": 707,
                    "log": "/tmp/issue-7.log",
                    "startedAt": 100,
                    "title": "quota failure",
                }
            },
            "issues": {},
        }

        runner.prune_active_workers(
            state,
            pid_exists=lambda _pid: False,
            now=200,
            read_worker_result=lambda _worker: runner.WorkerResult(
                exit_code=1,
                error="ERROR: unexpected status 403 Forbidden: 用户额度不足",
            ),
            max_attempts=3,
            retry_backoff_seconds=600,
            capacity_backoff_seconds=1800,
        )

        self.assertEqual(state["issues"]["7"]["nextRetryAt"], 2000)

    def test_model_channel_errors_are_resource_errors(self):
        self.assertTrue(
            runner.is_resource_backoff_error(
                "ERROR: unexpected status 503 Service Unavailable: no forced GPT backup channel supports group svip model gpt-5.5"
            )
        )

    def test_successful_worker_waits_before_non_terminal_retry(self):
        state = {
            "workers": {
                "7": {
                    "pid": 707,
                    "log": "/tmp/issue-7.log",
                    "startedAt": 100,
                    "title": "incomplete success",
                }
            },
            "issues": {},
        }

        runner.prune_active_workers(
            state,
            pid_exists=lambda _pid: False,
            now=200,
            read_worker_result=lambda _worker: runner.WorkerResult(exit_code=0),
            max_attempts=3,
            retry_backoff_seconds=600,
            capacity_backoff_seconds=1800,
        )

        meta = state["issues"]["7"]
        self.assertEqual(meta["lastExitCode"], 0)
        self.assertEqual(meta["nextRetryAt"], 800)
        self.assertEqual(meta.get("attempts", 0), 0)

    def test_prune_active_workers_pauses_after_max_failures(self):
        state = {
            "workers": {
                "7": {"pid": 707, "log": "/tmp/issue-7.log", "startedAt": 100}
            },
            "issues": {
                "7": {"attempts": 2}
            },
        }

        runner.prune_active_workers(
            state,
            pid_exists=lambda _pid: False,
            now=200,
            read_worker_result=lambda _worker: runner.WorkerResult(exit_code=1, error="failed"),
            max_attempts=3,
            retry_backoff_seconds=300,
            capacity_backoff_seconds=900,
        )

        meta = state["issues"]["7"]
        self.assertEqual(meta["attempts"], 3)
        self.assertTrue(meta["paused"])
        self.assertEqual(meta["pauseReason"], "failed 3 attempts")

    def test_filter_retryable_candidates_waits_for_backoff_and_paused_issues(self):
        issues = [
            {"number": "7", "title": "waiting", "labels": []},
            {"number": "8", "title": "paused", "labels": []},
            {"number": "9", "title": "ready", "labels": []},
        ]
        state = {
            "issues": {
                "7": {"nextRetryAt": 500},
                "8": {"paused": True},
            }
        }

        filtered = runner.filter_retryable_candidates(issues, state, now=400)

        self.assertEqual([issue["number"] for issue in filtered], ["9"])

    def test_issue_is_processable_enforces_min_restart_interval(self):
        issue = {"number": "7", "title": "running", "labels": [{"name": "处理中"}]}
        state = {
            "issues": {
                "7": {
                    "attempts": 1,
                    "lastStartedAt": 100,
                    "nextRetryAt": 0,
                }
            }
        }

        self.assertFalse(
            runner.issue_is_processable(
                issue,
                state,
                runner.DEFAULT_SKIP_LABELS,
                now=500,
                min_restart_seconds=600,
            )
        )
        self.assertTrue(
            runner.issue_is_processable(
                issue,
                state,
                runner.DEFAULT_SKIP_LABELS,
                now=701,
                min_restart_seconds=600,
            )
        )

    def test_render_progress_summary_includes_active_and_paused_counts(self):
        state = {
            "workers": {
                "7": {"title": "running issue", "startedAt": 100},
            },
            "issues": {
                "8": {"paused": True, "attempts": 3, "lastError": "failed 3 attempts"},
            },
        }
        issues = [
            {"number": "7", "title": "running issue", "labels": [{"name": "处理中"}]},
            {"number": "8", "title": "paused issue", "labels": []},
        ]

        summary = runner.render_progress_summary(issues, state, now=160)

        self.assertIn("Active workers: 1", summary)
        self.assertIn("#7 running issue", summary)
        self.assertIn("Paused issues: 1", summary)
        self.assertIn("#8 attempts=3", summary)

    def test_render_progress_summary_skips_waiting_for_active_or_closed_issues(self):
        state = {
            "workers": {
                "7": {"title": "running issue", "startedAt": 100},
            },
            "issues": {
                "7": {"nextRetryAt": 500, "title": "running issue"},
                "8": {"nextRetryAt": 500, "title": "waiting issue"},
                "9": {"nextRetryAt": 500, "title": "closed issue"},
                "10": {"paused": True, "attempts": 3, "title": "done issue"},
            },
        }
        issues = [
            {"number": "7", "title": "running issue", "labels": [{"name": "处理中"}]},
            {"number": "8", "title": "waiting issue", "labels": [{"name": "处理中"}]},
            {"number": "10", "title": "done issue", "labels": [{"name": "已处理"}]},
        ]

        summary = runner.render_progress_summary(issues, state, now=160)

        self.assertIn("#8 retry_in=340s", summary)
        self.assertNotIn("#7 retry_in", summary)
        self.assertNotIn("#9", summary)
        self.assertNotIn("#10", summary)

    def test_available_capacity_waits_when_five_ai_workers_are_active(self):
        state = {
            "workers": {
                str(index): {"pid": 1000 + index}
                for index in range(5)
            }
        }

        capacity = runner.available_capacity(state, max_concurrency=5, pid_exists=lambda _pid: True)

        self.assertEqual(capacity, 0)

    def test_remove_terminal_issue_workers_frees_processed_slots(self):
        state = {
            "workers": {
                "5": {"pid": 1005},
                "8": {"pid": 1008},
            }
        }
        issues = [
            {"number": "5", "labels": [{"name": "已处理"}]},
            {"number": "8", "labels": [{"name": "处理中"}]},
        ]
        killed = []

        runner.remove_terminal_issue_workers(
            state,
            issues,
            terminal_labels={"已处理"},
            terminate_worker=lambda pid: killed.append(pid),
        )

        self.assertEqual(state["workers"], {"8": {"pid": 1008}})
        self.assertEqual(killed, [1005])


if __name__ == "__main__":
    unittest.main()
