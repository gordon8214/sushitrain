// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
struct RecycleSink : implements<RecycleSink,IFileOperationProgressSink> {
    HRESULT result{E_ABORT};
    HRESULT __stdcall StartOperations() noexcept override{return S_OK;}
    HRESULT __stdcall FinishOperations(HRESULT status) noexcept override{if(FAILED(status))result=status;return S_OK;}
    HRESULT __stdcall PreRenameItem(DWORD,IShellItem*,LPCWSTR) noexcept override{return E_NOTIMPL;}
    HRESULT __stdcall PostRenameItem(DWORD,IShellItem*,LPCWSTR,HRESULT,IShellItem*) noexcept override{return S_OK;}
    HRESULT __stdcall PreMoveItem(DWORD,IShellItem*,IShellItem*,LPCWSTR) noexcept override{return E_NOTIMPL;}
    HRESULT __stdcall PostMoveItem(DWORD,IShellItem*,IShellItem*,LPCWSTR,HRESULT,IShellItem*) noexcept override{return S_OK;}
    HRESULT __stdcall PreCopyItem(DWORD,IShellItem*,IShellItem*,LPCWSTR) noexcept override{return E_NOTIMPL;}
    HRESULT __stdcall PostCopyItem(DWORD,IShellItem*,IShellItem*,LPCWSTR,HRESULT,IShellItem*) noexcept override{return S_OK;}
    HRESULT __stdcall PreDeleteItem(DWORD flags,IShellItem*) noexcept override{return flags&TSF_DELETE_RECYCLE_IF_POSSIBLE ? S_OK : E_ABORT;}
    HRESULT __stdcall PostDeleteItem(DWORD,IShellItem*,HRESULT status,IShellItem* recycled) noexcept override{result=FAILED(status)?status:(recycled?S_OK:E_ABORT);return result;}
    HRESULT __stdcall PreNewItem(DWORD,IShellItem*,LPCWSTR) noexcept override{return E_NOTIMPL;}
    HRESULT __stdcall PostNewItem(DWORD,IShellItem*,LPCWSTR,LPCWSTR,DWORD,HRESULT,IShellItem*) noexcept override{return S_OK;}
    HRESULT __stdcall UpdateProgress(UINT,UINT) noexcept override{return S_OK;}
    HRESULT __stdcall ResetTimer() noexcept override{return S_OK;}
    HRESULT __stdcall PauseTimer() noexcept override{return S_OK;}
    HRESULT __stdcall ResumeTimer() noexcept override{return S_OK;}
};
int recycleItem(std::wstring const& path) {
    com_ptr<IFileOperation> operation;check_hresult(CoCreateInstance(CLSID_FileOperation,nullptr,CLSCTX_INPROC_SERVER,IID_PPV_ARGS(operation.put())));
    check_hresult(operation->SetOperationFlags(FOF_NOCONFIRMATION|FOF_NO_CONNECTED_ELEMENTS|FOF_NOERRORUI|FOF_SILENT|FOF_WANTNUKEWARNING|FOFX_RECYCLEONDELETE|FOFX_ADDUNDORECORD|FOFX_EARLYFAILURE));
    com_ptr<IShellItem> item;check_hresult(SHCreateItemFromParsingName(fs::absolute(path).c_str(),nullptr,IID_PPV_ARGS(item.put())));
    auto sink=make_self<RecycleSink>();check_hresult(operation->DeleteItem(item.get(),sink.get()));check_hresult(operation->PerformOperations());
    BOOL canceled{};check_hresult(operation->GetAnyOperationsAborted(&canceled));if(canceled)throw hresult_error(E_ABORT,L"Windows could not move the item to the Recycle Bin.");check_hresult(sink->result);return 0;
}
