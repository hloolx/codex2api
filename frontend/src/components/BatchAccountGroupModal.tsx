import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { api } from "../api";
import { useToast } from "../hooks/useToast";
import type { AccountGroup, UpstreamChannel } from "../types";
import { getErrorMessage } from "../utils/error";
import AccountGroupMultiSelect from "./AccountGroupMultiSelect";
import { ACCOUNT_GROUP_COLORS } from "./AccountGroupManagerModal";
import Modal from "./Modal";
import { Button } from "@/components/ui/button";

// 批量设置账号分组(issue #763):Grok / Antigravity / Claude 页共用,语义与 Codex
// 页"设置分组"一致——所选分组完整替换,留空即清空;走 batch-update,后端校验渠道。
export default function BatchAccountGroupModal({
  show,
  ids,
  channel,
  groups,
  onClose,
  onSaved,
  onGroupsChanged,
}: {
  show: boolean;
  ids: number[];
  channel: UpstreamChannel;
  groups: AccountGroup[];
  onClose: () => void;
  onSaved: () => void | Promise<void>;
  onGroupsChanged?: () => unknown;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [groupIds, setGroupIds] = useState<number[]>([]);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (show) setGroupIds([]);
  }, [show]);

  const createGroupInline = useCallback(
    async (name: string): Promise<number | null> => {
      try {
        const color =
          ACCOUNT_GROUP_COLORS[groups.length % ACCOUNT_GROUP_COLORS.length];
        const res = await api.createAccountGroup({
          name: name.trim(),
          channel,
          color,
        });
        await onGroupsChanged?.();
        return res.id ?? null;
      } catch (error) {
        showToast(getErrorMessage(error), "error");
        return null;
      }
    },
    [channel, groups.length, onGroupsChanged, showToast],
  );

  const save = async () => {
    if (ids.length === 0) return;
    setSubmitting(true);
    try {
      const result = await api.batchUpdateAccounts({
        ids: [...ids],
        group_ids: [...groupIds],
      });
      showToast(
        t("accounts.batchMetaDone", {
          success: result.success,
          fail: result.failed,
        }),
      );
      await onSaved();
    } catch (error) {
      showToast(
        t("accounts.batchMetaFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setSubmitting(false);
    }
  };

  const close = () => {
    if (submitting) return;
    onClose();
  };

  return (
    <Modal
      show={show}
      title={t("accounts.batchGroupTitle")}
      contentClassName="sm:max-w-[520px]"
      onClose={close}
      footer={
        <>
          <Button
            type="button"
            variant="outline"
            disabled={submitting}
            onClick={close}
          >
            {t("common.cancel")}
          </Button>
          <Button
            type="button"
            disabled={submitting || ids.length === 0}
            onClick={() => void save()}
          >
            {submitting
              ? t("common.saving")
              : groupIds.length === 0
                ? t("accounts.batchGroupClear")
                : t("accounts.batchGroupReplace")}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <div className="rounded-lg border border-border bg-muted/20 p-3 text-sm text-muted-foreground">
          {t("accounts.batchGroupDesc", { count: ids.length })}
        </div>
        <AccountGroupMultiSelect
          groups={groups}
          value={groupIds}
          onChange={setGroupIds}
          allLabel={t("accounts.groupsUnbound")}
          selectedLabel={t("accounts.groupsSelected", {
            count: groupIds.length,
          })}
          placeholder={t("accounts.groupsPlaceholder")}
          emptyLabel={t("accounts.groupsNone")}
          emptyHint={t("accounts.groupsSelectHint")}
          onCreateGroup={onGroupsChanged ? createGroupInline : undefined}
          createLabel={t("accounts.groupCreate")}
          createPlaceholder={t("accounts.groupNamePlaceholder")}
          creatingLabel={t("accounts.groupCreating")}
          createEmptyHint={t("accounts.groupCreateInlineEmptyHint")}
        />
      </div>
    </Modal>
  );
}
