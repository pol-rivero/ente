import { appHomeRoute } from "ente-accounts/services/redirect";
import { LoadingIndicator } from "ente-base/components/loaders";
import { savedAuthToken } from "ente-base/token";
import { useRouter } from "next/router";
import React, { useEffect } from "react";

const Page: React.FC = () => {
    const router = useRouter();

    useEffect(() => {
        void savedAuthToken().then((token) =>
            router.replace(token ? appHomeRoute : "/login"),
        );
    }, [router]);

    return <LoadingIndicator />;
};

export default Page;
