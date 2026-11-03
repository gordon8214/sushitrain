// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
class DirectoryWatcher {
    handle directory,event;
    std::atomic<bool> ending{};
    std::thread thread;
public:
    explicit DirectoryWatcher(fs::path const& path) {
        directory.attach(CreateFileW(windowsPath(path).c_str(),FILE_LIST_DIRECTORY,FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE,nullptr,OPEN_EXISTING,FILE_FLAG_BACKUP_SEMANTICS|FILE_FLAG_OVERLAPPED,nullptr));
        if(directory.get()==INVALID_HANDLE_VALUE)throw_last_error();event.attach(CreateEventW(nullptr,TRUE,FALSE,nullptr));check_bool(static_cast<bool>(event));
        thread=std::thread([this]{
            std::array<std::byte,65536> buffer{};
            while(!ending){
                OVERLAPPED pending{};pending.hEvent=event.get();ResetEvent(event.get());DWORD bytes{};
                if(!ReadDirectoryChangesW(directory.get(),buffer.data(),static_cast<DWORD>(buffer.size()),TRUE,FILE_NOTIFY_CHANGE_FILE_NAME|FILE_NOTIFY_CHANGE_DIR_NAME|FILE_NOTIFY_CHANGE_SIZE|FILE_NOTIFY_CHANGE_LAST_WRITE|FILE_NOTIFY_CHANGE_ATTRIBUTES,nullptr,&pending,nullptr)){rescan=true;return;}
                while(!ending && WaitForSingleObject(event.get(),500)==WAIT_TIMEOUT){}
                if(ending){CancelIoEx(directory.get(),&pending);GetOverlappedResult(directory.get(),&pending,&bytes,TRUE);return;}
                // Zero bytes indicates a buffer overflow; a complete rescan is
                // also the recovery path for that case.
                GetOverlappedResult(directory.get(),&pending,&bytes,TRUE);rescan=true;
            }
        });
    }
    ~DirectoryWatcher(){ending=true;CancelIoEx(directory.get(),nullptr);if(thread.joinable())thread.join();}
    DirectoryWatcher(DirectoryWatcher const&)=delete;DirectoryWatcher& operator=(DirectoryWatcher const&)=delete;
};
