<?php
declare(strict_types=1);

namespace App\Api\V1\Controller;

use App\Api\V1\DTO\ApiResponse;
use App\Api\V1\Enum\StatusEnum;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\HttpFoundation\{JsonResponse, Response};

abstract class AbstractApiController extends AbstractController
{
    protected const DEFAULT_SERIALIZER_GROUPS = ['api'];

    protected function createJsonResponse($data = null, array $groups = [], int $code = Response::HTTP_OK, ?string $message = null, StatusEnum $status = StatusEnum::Success): JsonResponse
    {
        return $this->json(new ApiResponse($data, $code, $message, $status), $code, [], [
            'groups' => array_merge(self::DEFAULT_SERIALIZER_GROUPS,$groups),
        ]);
    }
}
